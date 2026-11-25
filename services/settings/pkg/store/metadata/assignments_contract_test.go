package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opencloud-eu/opencloud/services/settings/pkg/settings"
	rdefaults "github.com/opencloud-eu/opencloud/services/settings/pkg/store/defaults"
	"github.com/opencloud-eu/reva/v3/pkg/errtypes"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// barrier blocks callers until n of them have arrived, or until timeout.
// The timeout keeps a test from hanging if the store serialises the callers
// (which is the fix we would want); in that case they pass one at a time.
type barrier struct {
	mu      sync.Mutex
	n       int
	arrived int
	release chan struct{}
	timeout time.Duration
}

func newBarrier(n int, timeout time.Duration) *barrier {
	return &barrier{n: n, release: make(chan struct{}), timeout: timeout}
}

func (b *barrier) wait() {
	b.mu.Lock()
	b.arrived++
	if b.arrived == b.n {
		close(b.release)
	}
	b.mu.Unlock()
	select {
	case <-b.release:
	case <-time.After(b.timeout):
	}
}

// C1: two concurrent WriteRoleAssignment calls for the same user must still
// leave exactly one role. WriteRoleAssignment is delete-then-upload with no
// lock, so the barrier holds both writers after their delete and before their
// upload; both files then survive.
func TestWriteRoleAssignment_ConcurrentWritesKeepOneRole(t *testing.T) {
	s := initStore()
	setupRoles(s)
	m := mockOf(t, s)

	b := newBarrier(2, 2*time.Second)
	m.OnCall("SimpleUpload",
		func(p string) bool { return isUnder(p, accountPath(alan)) },
		func(string) error { b.wait(); return nil })

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, role := range []string{roleA, roleB} {
		wg.Add(1)
		go func(role string) {
			defer wg.Done()
			_, err := s.WriteRoleAssignment(alan, role)
			errs <- err
		}(role)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	list, err := s.ListRoleAssignments(alan)
	require.NoError(t, err)
	require.Len(t, list, 1, "concurrent writes left %d role assignments for one user", len(list))
	require.Len(t, m.Files(accountPath(alan)), 1)
}

// X1: ListRoleAssignmentsByRole lists the account folders, then reads each one.
// If one folder is gone by the time it is read (e.g. RemoveRoleAssignment
// deleted it in between), that account should be skipped. assignments.go:89
// instead returns an empty result for the whole query.
func TestListRoleAssignmentsByRole_SkipsVanishedAccount(t *testing.T) {
	s := initStore()
	setupRoles(s)
	m := mockOf(t, s)

	_, err := s.WriteRoleAssignment(alan, role1)
	require.NoError(t, err)
	_, err = s.WriteRoleAssignment(mary, role1)
	require.NoError(t, err)

	// alan sorts before mary, so his folder is read first
	m.FailOn("ReadDir", accountPath(alan), errtypes.NotFound(accountPath(alan)))

	list, err := s.ListRoleAssignmentsByRole(role1)
	require.NoError(t, err)
	accounts := make([]string, 0, len(list))
	for _, a := range list {
		accounts = append(accounts, a.AccountUuid)
	}
	require.Equal(t, []string{mary}, accounts,
		"one vanished account folder must not hide the other accounts")
}

// S1: service accounts never touch storage. ListRoleAssignments answers with
// the hidden service-account role, and ReadBundle serves that role from code.
func TestServiceAccountShortcut(t *testing.T) {
	const svcAccount = "service-account-1"

	s := initStore()
	setupRoles(s)
	s.cfg.ServiceAccountIDs = []string{"other-service", svcAccount}
	m := mockOf(t, s)

	// a stored assignment for the service account must be ignored
	m.seedAssignment(t, svcAccount, "00000000-0000-0000-0000-0000000000c1", rdefaults.BundleUUIDRoleAdmin)
	m.ResetCalls()

	list, err := s.ListRoleAssignments(svcAccount)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, svcAccount, list[0].AccountUuid)
	require.Equal(t, rdefaults.BundleUUIDServiceAccount, list[0].RoleId)
	require.NotEmpty(t, list[0].Id)
	// Note: the id is a fresh uuid on every call (assignments.go:23), so it can
	// never be used with RemoveRoleAssignment.

	bundle, err := s.ReadBundle(rdefaults.BundleUUIDServiceAccount)
	require.NoError(t, err)
	require.True(t, proto.Equal(rdefaults.ServiceAccountBundle(), bundle))

	require.Empty(t, m.Calls(), "the service-account shortcut must not call storage")

	// a regular account still goes to storage
	_, err = s.ListRoleAssignments(alan)
	require.NoError(t, err)
	var readDirs []string
	for _, c := range m.Calls() {
		if c.Op == "ReadDir" {
			readDirs = append(readDirs, c.Path)
		}
	}
	require.Equal(t, []string{accountPath(alan)}, readDirs)
	require.False(t, strings.Contains(strings.Join(readDirs, ","), svcAccount))
}

// F3: removing an id that was never written reports ErrNotFound.
func TestRemoveRoleAssignment_UnknownID(t *testing.T) {
	s := initStore()
	setupRoles(s)
	_, err := s.WriteRoleAssignment(alan, roleA)
	require.NoError(t, err)

	err = s.RemoveRoleAssignment("00000000-0000-0000-0000-00000000beef")
	require.ErrorIs(t, err, settings.ErrNotFound)
	requireSingleAssignment(t, s, alan, roleA)
}

// F4: an account or role nobody has used yields an empty list, not an error.
// This relies on the fake returning NotFound for a missing folder, like the real client.
func TestListRoleAssignments_UnknownAccount(t *testing.T) {
	s := initStore()
	setupRoles(s)
	_, err := s.WriteRoleAssignment(alan, role1)
	require.NoError(t, err)

	list, err := s.ListRoleAssignments("00000000-0000-0000-0000-00000000f404")
	require.NoError(t, err)
	require.NotNil(t, list)
	require.Empty(t, list)

	list, err = s.ListRoleAssignmentsByRole("00000000-0000-0000-0000-00000000f404")
	require.NoError(t, err)
	require.NotNil(t, list)
	require.Empty(t, list)
}

// F9: an account configured as AdminUserID that already holds the User role
// ends up with exactly one role, Admin, after initialisation.
func TestAdminUserIDInit_PreloadedUserBecomesAdmin(t *testing.T) {
	s, m := newBootstrapStore(t)
	s.cfg.AdminUserID = alan
	m.seedAssignment(t, alan, "00000000-0000-0000-0000-0000000000d1", rdefaults.BundleUUIDRoleUser)

	require.NoError(t, s.initMetadataClient(m))

	requireSingleAssignment(t, s, alan, rdefaults.BundleUUIDRoleAdmin)
}

// S2: WriteRoleAssignment clears the account folder before uploading the new
// assignment; the reverse order would delete the assignment it just wrote.
func TestWriteRoleAssignment_DeletesBeforeUpload(t *testing.T) {
	s := initStore()
	setupRoles(s)
	m := mockOf(t, s)
	m.ResetCalls()

	a, err := s.WriteRoleAssignment(alan, roleA)
	require.NoError(t, err)

	deleteAt, uploadAt := -1, -1
	for i, c := range m.Calls() {
		switch {
		case c.Op == "Delete" && c.Path == accountPath(alan):
			deleteAt = i
		case c.Op == "SimpleUpload" && c.Path == assignmentPath(alan, a.Id):
			uploadAt = i
		}
	}
	require.NotEqual(t, -1, deleteAt, "Delete(%s) was not called", accountPath(alan))
	require.NotEqual(t, -1, uploadAt, "SimpleUpload(%s) was not called", assignmentPath(alan, a.Id))
	require.Less(t, deleteAt, uploadAt)
}

// S3: ListRoleAssignmentsByRole reads every account folder, so it makes
// N+1 ReadDir calls for N accounts. This documents the cost growing with N.
func TestListRoleAssignmentsByRole_ReadDirCost(t *testing.T) {
	for _, n := range []int{1, 5, 25} {
		t.Run(fmt.Sprintf("%d accounts", n), func(t *testing.T) {
			s := initStore()
			setupRoles(s)
			m := mockOf(t, s)
			for i := 0; i < n; i++ {
				_, err := s.WriteRoleAssignment(fmt.Sprintf("acc-%03d", i), role1)
				require.NoError(t, err)
			}
			accounts, err := m.ReadDir(context.Background(), accountsFolderLocation)
			require.NoError(t, err)
			require.Len(t, accounts, n, "no accounts other than the ones written here")
			m.ResetCalls()

			_, err = s.ListRoleAssignmentsByRole(role1)
			require.NoError(t, err)

			readDirs := 0
			for _, c := range m.Calls() {
				if c.Op == "ReadDir" {
					readDirs++
				}
			}
			require.Equal(t, n+1, readDirs)
		})
	}
}

// X2: a corrupt assignment file is reported, not silently dropped.
func TestListRoleAssignments_InvalidJSON(t *testing.T) {
	s := initStore()
	setupRoles(s)
	m := mockOf(t, s)
	require.NoError(t, m.SimpleUpload(context.Background(), assignmentPath(alan, "corrupt"), []byte("{not json")))

	_, err := s.ListRoleAssignments(alan)
	require.Error(t, err)

	_, err = s.ListRoleAssignmentsByRole(role1)
	require.Error(t, err)
}

// X3: if clearing the old assignment fails (other than NotFound), the write
// stops there: the error is returned and nothing is uploaded.
func TestWriteRoleAssignment_DeleteFails(t *testing.T) {
	s := initStore()
	setupRoles(s)
	m := mockOf(t, s)
	old, err := s.WriteRoleAssignment(alan, roleA)
	require.NoError(t, err)

	boom := errors.New("storage down")
	m.FailOn("Delete", accountPath(alan), boom)
	m.ResetCalls()

	_, err = s.WriteRoleAssignment(alan, roleB)
	require.ErrorIs(t, err, boom)
	for _, c := range m.Calls() {
		require.NotEqual(t, "SimpleUpload", c.Op, "nothing may be uploaded after Delete failed")
	}
	got := requireSingleAssignment(t, s, alan, roleA)
	require.Equal(t, old.Id, got.Id)
}

// X4: if the upload fails after the old assignment was deleted, the account is
// left with zero roles. This records current behaviour: the error is returned,
// but the "exactly one role" invariant is already broken.
func TestWriteRoleAssignment_UploadFailsAfterDelete(t *testing.T) {
	s := initStore()
	setupRoles(s)
	m := mockOf(t, s)
	_, err := s.WriteRoleAssignment(alan, roleA)
	require.NoError(t, err)

	boom := errors.New("storage down")
	m.OnCall("SimpleUpload", func(p string) bool { return isUnder(p, accountPath(alan)) },
		func(string) error { return boom })

	_, err = s.WriteRoleAssignment(alan, roleB)
	require.ErrorIs(t, err, boom)

	list, err := s.ListRoleAssignments(alan)
	require.NoError(t, err)
	require.Empty(t, list, "current behaviour: the old role is gone and the new one was never written")
}

// M1: RemoveRoleAssignment deletes the whole account folder, so removing one
// assignment also removes any other assignment stored for that account.
// Strict call sequence: list accounts, list alan's folder, delete it; nothing else.
func TestRemoveRoleAssignment_CallSequence(t *testing.T) {
	s := initStore()
	setupRoles(s)
	m := mockOf(t, s)

	a, err := s.WriteRoleAssignment(alan, roleA)
	require.NoError(t, err)
	_, err = s.WriteRoleAssignment(mary, roleB) // sorts after alan, must not be read
	require.NoError(t, err)
	// a second file in alan's folder, a state the public API does not produce
	extra := "00000000-0000-0000-0000-0000000000e1"
	m.seedAssignment(t, alan, extra, roleB)
	m.ResetCalls()

	require.NoError(t, s.RemoveRoleAssignment(a.Id))

	require.Equal(t, []mdcCall{
		{Op: "ReadDir", Path: accountsFolderLocation},
		{Op: "ReadDir", Path: accountPath(alan)},
		{Op: "Delete", Path: accountPath(alan)},
	}, m.Calls())
	require.False(t, m.IDExists(assignmentPath(alan, extra)),
		"the other assignment in alan's folder is deleted too")
	requireSingleAssignment(t, s, mary, roleB)
}
