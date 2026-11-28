package store

// Role-assignment and permission store (metadata.Store) — component test suite.
//
// Each test is tagged with its test-case id (TC-STORE-01 … TC-STORE-05) in its name and doc
// comment. The cases are derived from the contract in "OpenCloud Settings Store —
// Operation Contracts" (clause ids in brackets). Doubles live in rap_doubles_test.go.
//
// Run: ./run-tests.sh   (repo root)  or   go test -run 'TestTC_STORE_' -v .

import (
	"errors"
	"sort"
	"testing"

	"github.com/google/uuid"
	settingsmsg "github.com/opencloud-eu/opencloud/protogen/gen/opencloud/messages/settings/v0"
	"github.com/opencloud-eu/opencloud/services/settings/pkg/settings"
	rdefaults "github.com/opencloud-eu/opencloud/services/settings/pkg/store/defaults"
	"github.com/opencloud-eu/reva/v3/pkg/errtypes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	rapAlice   = "a1a1a1a1-0000-4000-8000-000000000001"
	rapBob     = "b2b2b2b2-0000-4000-8000-000000000002"
	rapCarol   = "c3c3c3c3-0000-4000-8000-000000000003"
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

// TC-STORE-03 · Write/Remove/List · state transition, valid path [I1, W-6, R-3]
// NoRole --Write(Admin)--> Role(Admin) --Write(User)--> Role(User) --Remove(id)--> NoRole
func TestTC_STORE_03_RoleLifecycle_StateTransitions(t *testing.T) {
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

// TC-STORE-04 · ListRoleAssignmentsByRole · exceptional: one account folder vanishes [LB-2, LB-6]
// Stub: ReadDir of Bob's folder answers NotFound (deleted between the two reads).
// Expected: Alice and Carol are still listed. Known defect: the result is empty.
//
// Strengthened after mutation testing: the defect makes the whole test fail, so on its own
// it could not tell a mutant from the original (19 surviving mutants in this function).
// healthy_storage now checks LB-2 on its own, and passes.
func TestTC_STORE_04_ListRoleAssignmentsByRole_OneFolderVanishes(t *testing.T) {
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

// TC-STORE-05 · ReadPermissionByName · decision table [PN-1, PN-3, P-4]
func TestTC_STORE_05_ReadPermissionByName_DecisionTable(t *testing.T) {
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
