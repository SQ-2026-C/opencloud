package store

// Role-assignment and permission store (metadata.Store) — component test suite.
//
// Each test is tagged with its test-case id (TC-01 … TC-10) in its name and doc
// comment. The cases are derived from the contract in "OpenCloud Settings Store —
// Operation Contracts" (clause ids in brackets). Doubles live in rap_doubles_test.go.
//
// Run: ./run-tests.sh   (repo root)  or   go test -run 'TestTC' -v .

import (
	"errors"
	"sort"
	"sync"
	"testing"

	"github.com/google/uuid"
	olog "github.com/opencloud-eu/opencloud/pkg/log"
	settingsmsg "github.com/opencloud-eu/opencloud/protogen/gen/opencloud/messages/settings/v0"
	"github.com/opencloud-eu/opencloud/services/settings/pkg/config"
	"github.com/opencloud-eu/opencloud/services/settings/pkg/settings"
	rdefaults "github.com/opencloud-eu/opencloud/services/settings/pkg/store/defaults"
	"github.com/opencloud-eu/reva/v3/pkg/errtypes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

const (
	rapAlice   = "a1a1a1a1-0000-4000-8000-000000000001"
	rapBob     = "b2b2b2b2-0000-4000-8000-000000000002"
	rapCarol   = "c3c3c3c3-0000-4000-8000-000000000003"
	rapAdmin   = "ad000000-0000-4000-8000-0000000000ad"
	rapService = "5e5e5e5e-0000-4000-8000-00000000005e"

	roleAdmin   = rdefaults.BundleUUIDRoleAdmin
	roleUser    = rdefaults.BundleUUIDRoleUser
	roleService = rdefaults.BundleUUIDServiceAccount
)

// write assigns a role through the store and fails the test on error.
func write(t *testing.T, s *Store, account, role string) *settingsmsg.UserRoleAssignment {
	t.Helper()
	a, err := s.WriteRoleAssignment(account, role)
	require.NoError(t, err)
	return a
}

// roles returns the role ids the account holds, sorted.
func roles(t *testing.T, s *Store, account string) []string {
	t.Helper()
	as, err := s.ListRoleAssignments(account)
	require.NoError(t, err)
	out := make([]string, 0, len(as))
	for _, a := range as {
		out = append(out, a.GetRoleId())
	}
	sort.Strings(out)
	return out
}

// TC-01 · ListRoleAssignments · equivalence partitioning of accountUUID [LA-3, LA-5]
// P1 account with a stored role, P2 account never assigned, P3 service account.
func TestTC01_ListRoleAssignments_EquivalencePartitions(t *testing.T) {
	spy := &spyMDC{next: newFakeMDC()}
	s := newStoreWith(t, spy, rapService)
	stored := write(t, s, rapAlice, roleUser)

	t.Run("P1_stored_role", func(t *testing.T) {
		got, err := s.ListRoleAssignments(rapAlice)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, stored.GetId(), got[0].GetId())
		assert.Equal(t, roleUser, got[0].GetRoleId())
	})
	t.Run("P2_never_assigned", func(t *testing.T) {
		got, err := s.ListRoleAssignments(rapBob)
		require.NoError(t, err)
		assert.NotNil(t, got)
		assert.Empty(t, got)
	})
	t.Run("P3_service_account", func(t *testing.T) {
		spy.calls = nil
		got, err := s.ListRoleAssignments(rapService)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, roleService, got[0].GetRoleId())
		assert.NoError(t, uuid.Validate(got[0].GetId()))
		assert.Empty(t, spy.calls, "service accounts must be answered without touching storage")
	})
}

// TC-02 · WriteRoleAssignment · boundary value: accountUUID of length 0 [W-1, I1]
// Expected: the empty id is rejected and every other account keeps its role.
// Known defect: the store deletes settings/accounts/ and reports success.
func TestTC02_WriteRoleAssignment_EmptyAccountID(t *testing.T) {
	spy := &spyMDC{next: newFakeMDC()}
	s := newStoreWith(t, spy)
	write(t, s, rapAlice, roleUser)
	write(t, s, rapBob, roleAdmin)
	spy.calls = nil

	_, err := s.WriteRoleAssignment("", roleUser)

	for _, c := range spy.calls {
		if c.op == "Delete" {
			t.Logf("store called Delete(%q)", c.path)
		}
	}
	assert.Error(t, err, "an empty account id must be rejected")
	assert.Equal(t, []string{roleUser}, roles(t, s, rapAlice), "Alice must keep her role")
	assert.Equal(t, []string{roleAdmin}, roles(t, s, rapBob), "Bob must keep his role")
}

// TC-03 · WriteRoleAssignment · boundary value: accountUUID of length 1, next to a
// prefix-sharing account [W-6, W-9]
func TestTC03_WriteRoleAssignment_OneCharAccountID(t *testing.T) {
	s := newStoreWith(t, newFakeMDC())
	write(t, s, "ab", roleAdmin)

	got := write(t, s, "a", roleUser)

	assert.Equal(t, "a", got.GetAccountUuid())
	assert.Equal(t, []string{roleUser}, roles(t, s, "a"))
	assert.Equal(t, []string{roleAdmin}, roles(t, s, "ab"), "prefix-sharing account must be untouched")
}

// TC-04 · Write/Remove/List · state transition, valid path [I1, W-6, R-3]
// NoRole --Write(Admin)--> Role(Admin) --Write(User)--> Role(User) --Remove(id)--> NoRole
func TestTC04_RoleLifecycle_StateTransitions(t *testing.T) {
	seededUUIDs(t)
	s := newStoreWith(t, newFakeMDC())

	assert.Empty(t, roles(t, s, rapAlice), "S0 NoRole")

	a1 := write(t, s, rapAlice, roleAdmin)
	assert.Equal(t, []string{roleAdmin}, roles(t, s, rapAlice), "S1 Role(Admin)")

	a2 := write(t, s, rapAlice, roleUser)
	assert.Equal(t, []string{roleUser}, roles(t, s, rapAlice), "S2 Role(User): replaced, not appended")
	assert.NotEqual(t, a1.GetId(), a2.GetId(), "each write gets a fresh id")

	require.NoError(t, s.RemoveRoleAssignment(a2.GetId()))
	assert.Empty(t, roles(t, s, rapAlice), "S3 NoRole")
}

// TC-05 · RemoveRoleAssignment · state transition, invalid events [R-1, R-6]
// Remove of an unknown id, and of a stale id that a later write replaced.
func TestTC05_RemoveRoleAssignment_InvalidTransitions(t *testing.T) {
	s := newStoreWith(t, newFakeMDC())
	stale := write(t, s, rapAlice, roleAdmin)
	write(t, s, rapAlice, roleUser)

	for name, id := range map[string]string{
		"unknown_id": uuid.NewString(),
		"stale_id":   stale.GetId(),
	} {
		t.Run(name, func(t *testing.T) {
			err := s.RemoveRoleAssignment(id)
			assert.ErrorIs(t, err, settings.ErrNotFound)
			assert.Equal(t, []string{roleUser}, roles(t, s, rapAlice), "current role must survive")
		})
	}
}

// TC-06 · WriteRoleAssignment · exceptional: Delete fails [W-12]
// Mock: Delete is expected once; MakeDirIfNotExist and SimpleUpload must not be called.
func TestTC06_WriteRoleAssignment_DeleteFails(t *testing.T) {
	m := &mockMDC{}
	m.On("Delete", accountPath(rapAlice)).Return(errors.New("storage unavailable")).Once()
	s := newStoreWith(t, m)

	got, err := s.WriteRoleAssignment(rapAlice, roleUser)

	assert.Error(t, err)
	assert.Nil(t, got)
	m.AssertExpectations(t)
	m.AssertNotCalled(t, "MakeDirIfNotExist", mock.Anything)
	m.AssertNotCalled(t, "SimpleUpload", mock.Anything, mock.Anything)
}

// TC-07 · WriteRoleAssignment · exceptional: upload fails after the delete [W-13]
// Stub: every SimpleUpload returns a canned error.
func TestTC07_WriteRoleAssignment_UploadFails(t *testing.T) {
	fake := newFakeMDC()
	write(t, newStoreWith(t, fake), rapAlice, roleAdmin)
	s := newStoreWith(t, &stubMDC{next: fake, op: "SimpleUpload", err: errors.New("quota exceeded")})

	got, err := s.WriteRoleAssignment(rapAlice, roleUser)

	assert.Error(t, err)
	assert.NotNil(t, got, "the unsaved assignment is returned together with the error")
	assert.Empty(t, roles(t, s, rapAlice), "old role already deleted: account left with zero roles")
}

// TC-08 · ListRoleAssignmentsByRole · exceptional: one account folder vanishes [LB-2, LB-6]
// Stub: ReadDir of Bob's folder answers NotFound (deleted between the two reads).
// Expected: Alice and Carol are still listed. Known defect: the result is empty.
func TestTC08_ListRoleAssignmentsByRole_OneFolderVanishes(t *testing.T) {
	fake := newFakeMDC()
	seed := newStoreWith(t, fake)
	write(t, seed, rapAlice, roleUser)
	write(t, seed, rapBob, roleUser)
	write(t, seed, rapCarol, roleUser)
	s := newStoreWith(t, &stubMDC{next: fake, op: "ReadDir", path: accountPath(rapBob), err: errtypes.NotFound("gone")})

	got, err := s.ListRoleAssignmentsByRole(roleUser)

	require.NoError(t, err)
	accounts := []string{}
	for _, a := range got {
		accounts = append(accounts, a.GetAccountUuid())
	}
	assert.ElementsMatch(t, []string{rapAlice, rapCarol}, accounts)
}

// TC-09 · ReadPermissionByName · decision table [PN-1, PN-3, P-4]
func TestTC09_ReadPermissionByName_DecisionTable(t *testing.T) {
	const (
		perm       = "Drives.Create"
		roleWith   = "11111111-0000-4000-8000-000000000001"
		roleNoPerm = "22222222-0000-4000-8000-000000000002"
		roleGone   = "33333333-0000-4000-8000-000000000003"
		roleBroken = "44444444-0000-4000-8000-000000000004"
	)
	want := &settingsmsg.Permission{
		Operation:  settingsmsg.Permission_OPERATION_READWRITE,
		Constraint: settingsmsg.Permission_CONSTRAINT_ALL,
	}
	bundle := func(id string, settings ...*settingsmsg.Setting) *settingsmsg.Bundle {
		return &settingsmsg.Bundle{Id: id, Type: settingsmsg.Bundle_TYPE_ROLE, Name: id, Settings: settings}
	}
	fake := newFakeMDC()
	seed := newStoreWith(t, fake)
	for _, b := range []*settingsmsg.Bundle{
		bundle(roleWith, &settingsmsg.Setting{Id: "p1", Name: perm,
			Value: &settingsmsg.Setting_PermissionValue{PermissionValue: want}}),
		bundle(roleNoPerm),
		bundle(roleBroken),
	} {
		_, err := seed.WriteBundle(b)
		require.NoError(t, err)
	}
	s := newStoreWith(t, &stubMDC{next: fake, op: "SimpleDownload", path: bundlePath(roleBroken), err: errors.New("i/o timeout")})

	rules := []struct {
		name     string
		role     string
		wantPerm bool
	}{
		{"R1_readable_and_present", roleWith, true},
		{"R2_readable_not_present", roleNoPerm, false},
		{"R3_role_not_found", roleGone, false},
		{"R4_storage_error", roleBroken, false},
	}
	for _, r := range rules {
		t.Run(r.name, func(t *testing.T) {
			got, err := s.ReadPermissionByName(perm, []string{r.role})
			if r.wantPerm {
				require.NoError(t, err)
				assert.Equal(t, want.GetOperation(), got.GetOperation())
				assert.Equal(t, want.GetConstraint(), got.GetConstraint())
				return
			}
			assert.Nil(t, got)
			assert.ErrorIs(t, err, settings.ErrNotFound)
		})
	}
}

// TC-10 · initMetadataClient · admin bootstrap [IN-7, I6]
// The configured admin holds the User role before init; afterwards it holds only Admin.
func TestTC10_Init_AdminBootstrap(t *testing.T) {
	fake := newFakeMDC()
	write(t, newStoreWith(t, fake), rapAdmin, roleUser)
	s := &Store{
		Logger: olog.NopLogger(), // DUMMY
		cfg:    &config.Config{AdminUserID: rapAdmin},
		l:      &sync.Mutex{},
	}

	require.NoError(t, s.initMetadataClient(fake))

	assert.Equal(t, []string{roleAdmin}, roles(t, s, rapAdmin))
}
