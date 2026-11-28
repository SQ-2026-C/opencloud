package store

// Role-assignment and permission store (metadata.Store) — component test suite.
//
// Each test is tagged with its test-case id (TC-STORE-01 … TC-STORE-12) in its name and doc
// comment. The cases are derived from the contract in "OpenCloud Settings Store —
// Operation Contracts" (clause ids in brackets). Doubles live in rap_doubles_test.go.
//
// Run: ./run-tests.sh   (repo root)  or   go test -run 'TestTC_STORE_' -v .

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

// TC-STORE-01 · ListRoleAssignments · equivalence partitioning of accountUUID [LA-3, LA-5]
// P1 account with a stored role, P2 account never assigned, P3 service account.
func TestTC_STORE_01_ListRoleAssignments_EquivalencePartitions(t *testing.T) {
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

// TC-STORE-02 · WriteRoleAssignment · boundary value: accountUUID of length 0 [W-1, I1]
// Expected: the empty id is rejected and every other account keeps its role.
// Known defect: the store deletes settings/accounts/ and reports success.
func TestTC_STORE_02_WriteRoleAssignment_EmptyAccountID(t *testing.T) {
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

// TC-STORE-03 · WriteRoleAssignment · boundary value: accountUUID of length 1, next to a
// prefix-sharing account [W-6, W-9]
func TestTC_STORE_03_WriteRoleAssignment_OneCharAccountID(t *testing.T) {
	s := newStoreWith(t, newFakeMDC())
	write(t, s, "ab", roleAdmin)

	got := write(t, s, "a", roleUser)

	assert.Equal(t, "a", got.GetAccountUuid())
	assert.Equal(t, []string{roleUser}, roles(t, s, "a"))
	assert.Equal(t, []string{roleAdmin}, roles(t, s, "ab"), "prefix-sharing account must be untouched")
}

// TC-STORE-04 · Write/Remove/List · state transition, valid path [I1, W-6, R-3]
// NoRole --Write(Admin)--> Role(Admin) --Write(User)--> Role(User) --Remove(id)--> NoRole
func TestTC_STORE_04_RoleLifecycle_StateTransitions(t *testing.T) {
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

// TC-STORE-05 · RemoveRoleAssignment · state transition, invalid events [R-1, R-6]
// Remove of an unknown id, and of a stale id that a later write replaced.
func TestTC_STORE_05_RemoveRoleAssignment_InvalidTransitions(t *testing.T) {
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
	// Added after mutation testing (mutant A054): an empty storage has no accounts folder.
	t.Run("no_accounts_folder", func(t *testing.T) {
		empty := newStoreWith(t, newFakeMDC())
		assert.ErrorIs(t, empty.RemoveRoleAssignment(uuid.NewString()), settings.ErrNotFound)
	})
}

// TC-STORE-06 · WriteRoleAssignment · exceptional: Delete fails [W-12]
// Mock: Delete is expected once; MakeDirIfNotExist and SimpleUpload must not be called.
func TestTC_STORE_06_WriteRoleAssignment_DeleteFails(t *testing.T) {
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

// TC-STORE-07 · WriteRoleAssignment · exceptional: upload fails after the delete [W-13]
// Stub: every SimpleUpload returns a canned error.
func TestTC_STORE_07_WriteRoleAssignment_UploadFails(t *testing.T) {
	fake := newFakeMDC()
	write(t, newStoreWith(t, fake), rapAlice, roleAdmin)
	s := newStoreWith(t, &stubMDC{next: fake, op: "SimpleUpload", err: errors.New("quota exceeded")})

	got, err := s.WriteRoleAssignment(rapAlice, roleUser)

	assert.Error(t, err)
	assert.NotNil(t, got, "the unsaved assignment is returned together with the error")
	assert.Empty(t, roles(t, s, rapAlice), "old role already deleted: account left with zero roles")
}

// TC-STORE-08 · ListRoleAssignmentsByRole · exceptional: one account folder vanishes [LB-2, LB-6]
// Stub: ReadDir of Bob's folder answers NotFound (deleted between the two reads).
// Expected: Alice and Carol are still listed. Known defect: the result is empty.
//
// Strengthened after mutation testing: the defect makes the whole test fail, so on its own
// it could not tell a mutant from the original (19 surviving mutants in this function).
// healthy_storage now checks LB-2 on its own, and passes.
func TestTC_STORE_08_ListRoleAssignmentsByRole_OneFolderVanishes(t *testing.T) {
	accountsOf := func(as []*settingsmsg.UserRoleAssignment) []string {
		out := []string{}
		for _, a := range as {
			out = append(out, a.GetAccountUuid())
		}
		return out
	}

	t.Run("healthy_storage", func(t *testing.T) {
		s := newStoreWith(t, newFakeMDC())
		write(t, s, rapAlice, roleUser)
		write(t, s, rapBob, roleAdmin)
		write(t, s, rapCarol, roleUser)

		users, err := s.ListRoleAssignmentsByRole(roleUser)
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{rapAlice, rapCarol}, accountsOf(users), "only the User holders")

		unused, err := s.ListRoleAssignmentsByRole(roleService)
		require.NoError(t, err)
		assert.NotNil(t, unused)
		assert.Empty(t, unused)
	})

	t.Run("one_folder_vanishes", func(t *testing.T) {
		fake := newFakeMDC()
		seed := newStoreWith(t, fake)
		write(t, seed, rapAlice, roleUser)
		write(t, seed, rapBob, roleUser)
		write(t, seed, rapCarol, roleUser)
		s := newStoreWith(t, &stubMDC{next: fake, op: "ReadDir", path: accountPath(rapBob), err: errtypes.NotFound("gone")})

		got, err := s.ListRoleAssignmentsByRole(roleUser)

		require.NoError(t, err)
		assert.ElementsMatch(t, []string{rapAlice, rapCarol}, accountsOf(got))
	})
}

// TC-STORE-09 · ReadPermissionByName · decision table [PN-1, PN-3, P-4]
func TestTC_STORE_09_ReadPermissionByName_DecisionTable(t *testing.T) {
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
		// a different permission, so the name comparison is also seen false (gobco)
		bundle(roleNoPerm, &settingsmsg.Setting{Id: "p2", Name: "Drives.DeleteProject",
			Value: &settingsmsg.Setting_PermissionValue{PermissionValue: want}}),
		bundle(roleBroken),
	} {
		_, err := seed.WriteBundle(b)
		require.NoError(t, err)
	}
	s := newStoreWith(t, &stubMDC{next: fake, op: "SimpleDownload", path: bundlePath(roleBroken), err: errors.New("i/o timeout")})

	rules := []struct {
		name     string
		roles    []string
		wantPerm bool
	}{
		{"R1_readable_and_present", []string{roleWith}, true},
		{"R2_readable_not_present", []string{roleNoPerm}, false},
		{"R3_role_not_found", []string{roleGone}, false},
		{"R4_storage_error", []string{roleBroken}, false},
		// Added after mutation testing (mutant P019, continue -> break): an unreadable
		// role is skipped and the search goes on with the next role [P-4].
		{"R5_unreadable_then_present", []string{roleBroken, roleWith}, true},
	}
	for _, r := range rules {
		t.Run(r.name, func(t *testing.T) {
			got, err := s.ReadPermissionByName(perm, r.roles)
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

// TC-STORE-10 · initMetadataClient · admin bootstrap [IN-7, IN-8, I6]
// The configured admin holds the User role before init; afterwards it holds only Admin.
// Strengthened after mutation testing: the first boot (S016), and an admin who already
// holds Admin keeps the same assignment instead of having it deleted and rewritten (S051, S053).
func TestTC_STORE_10_Init_AdminBootstrap(t *testing.T) {
	initWith := func(t *testing.T, fake *fakeMDC) *Store {
		s := &Store{
			Logger: olog.NopLogger(), // DUMMY
			cfg:    &config.Config{AdminUserID: rapAdmin},
			l:      &sync.Mutex{},
		}
		require.NoError(t, s.initMetadataClient(fake))
		return s
	}

	// Added after mutation testing (mutant S016): the first boot, with no folder for the admin yet.
	t.Run("first_boot_creates_admin", func(t *testing.T) {
		s := initWith(t, newFakeMDC())
		assert.Equal(t, []string{roleAdmin}, roles(t, s, rapAdmin))
	})

	t.Run("holds_user_becomes_admin", func(t *testing.T) {
		fake := newFakeMDC()
		write(t, newStoreWith(t, fake), rapAdmin, roleUser)
		s := initWith(t, fake)
		assert.Equal(t, []string{roleAdmin}, roles(t, s, rapAdmin))
	})

	t.Run("already_admin_untouched", func(t *testing.T) {
		fake := newFakeMDC()
		before := write(t, newStoreWith(t, fake), rapAdmin, roleAdmin)
		s := initWith(t, fake)
		got, err := s.ListRoleAssignments(rapAdmin)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, before.GetId(), got[0].GetId(), "the existing Admin assignment is kept")
	})
}

// TC-STORE-11 · ListRoleAssignments · exceptional: storage faults [LA-8]
// Added after mutation testing (mutants A007, A008, A011, A015, A017): no test made the
// storage misbehave on this read path.
func TestTC_STORE_11_ListRoleAssignments_StorageFaults(t *testing.T) {
	t.Run("read_dir_error_is_returned", func(t *testing.T) {
		fake := newFakeMDC()
		write(t, newStoreWith(t, fake), rapAlice, roleUser)
		s := newStoreWith(t, &stubMDC{next: fake, op: "ReadDir", path: accountPath(rapAlice), err: errors.New("i/o timeout")})

		got, err := s.ListRoleAssignments(rapAlice)
		assert.Error(t, err)
		assert.Nil(t, got)
	})

	t.Run("corrupt_file_is_an_error", func(t *testing.T) {
		fake := newFakeMDC()
		a := write(t, newStoreWith(t, fake), rapAlice, roleUser)
		fake.files[clean(assignmentPath(rapAlice, a.GetId()))] = []byte("{not json")
		s := newStoreWith(t, fake)

		got, err := s.ListRoleAssignments(rapAlice)
		assert.Error(t, err)
		assert.Nil(t, got)
	})

	t.Run("vanished_file_is_skipped", func(t *testing.T) {
		fake := newFakeMDC()
		a := write(t, newStoreWith(t, fake), rapAlice, roleUser)
		s := newStoreWith(t, &stubMDC{next: fake, op: "SimpleDownload", path: assignmentPath(rapAlice, a.GetId()), err: errtypes.NotFound("gone")})

		got, err := s.ListRoleAssignments(rapAlice)
		require.NoError(t, err)
		assert.NotNil(t, got)
		assert.Empty(t, got)
	})
}

// TC-STORE-12 · ReadPermissionByID · decision table [PI-2, P-4]
// Added after mutation testing: ReadPermissionByID had no test at all (7 surviving mutants).
func TestTC_STORE_12_ReadPermissionByID_DecisionTable(t *testing.T) {
	const (
		permID   = "perm-create-drives"
		roleWith = "55555555-0000-4000-8000-000000000005"
		roleNone = "66666666-0000-4000-8000-000000000006"
		roleGone = "77777777-0000-4000-8000-000000000007"
	)
	want := &settingsmsg.Permission{
		Operation:  settingsmsg.Permission_OPERATION_READWRITE,
		Constraint: settingsmsg.Permission_CONSTRAINT_OWN,
	}
	s := newStoreWith(t, newFakeMDC())
	for _, b := range []*settingsmsg.Bundle{
		{Id: roleWith, Type: settingsmsg.Bundle_TYPE_ROLE, Name: roleWith, Settings: []*settingsmsg.Setting{
			{Id: "other-id", Name: "Other", Value: &settingsmsg.Setting_PermissionValue{PermissionValue: &settingsmsg.Permission{}}},
			{Id: permID, Name: "Drives.Create", Value: &settingsmsg.Setting_PermissionValue{PermissionValue: want}},
		}},
		{Id: roleNone, Type: settingsmsg.Bundle_TYPE_ROLE, Name: roleNone},
	} {
		_, err := s.WriteBundle(b)
		require.NoError(t, err)
	}

	t.Run("R1_present", func(t *testing.T) {
		got, err := s.ReadPermissionByID(permID, []string{roleWith})
		require.NoError(t, err)
		assert.Equal(t, want.GetConstraint(), got.GetConstraint())
	})
	t.Run("R2_not_present_is_nil_nil", func(t *testing.T) {
		got, err := s.ReadPermissionByID(permID, []string{roleNone})
		assert.NoError(t, err)
		assert.Nil(t, got)
	})
	t.Run("R3_missing_role_skipped", func(t *testing.T) {
		got, err := s.ReadPermissionByID(permID, []string{roleGone, roleWith})
		require.NoError(t, err)
		assert.Equal(t, want.GetConstraint(), got.GetConstraint())
	})
}
