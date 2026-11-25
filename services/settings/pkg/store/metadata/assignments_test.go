package store

import (
	"log"
	"sync"
	"testing"

	"github.com/google/uuid"
	olog "github.com/opencloud-eu/opencloud/pkg/log"
	"github.com/opencloud-eu/opencloud/pkg/shared"
	settingsmsg "github.com/opencloud-eu/opencloud/protogen/gen/opencloud/messages/settings/v0"
	"github.com/opencloud-eu/opencloud/services/settings/pkg/config/defaults"
	"github.com/opencloud-eu/opencloud/services/settings/pkg/settings"
	"github.com/stretchr/testify/require"
)

var (
	alan   = "00000000-0000-0000-0000-000000000001"
	mary   = "00000000-0000-0000-0000-000000000002"
        dennis = "00000000-0000-0000-0000-000000000003"

	role1 = "11111111-1111-1111-1111-111111111111"
	role2 = "22222222-2222-2222-2222-222222222222"

	logger = olog.NewLogger(
		olog.Color(true),
		olog.Pretty(true),
		olog.Level("info"),
	)

	bundles = []*settingsmsg.Bundle{
		{
			Id:          "f36db5e6-a03c-40df-8413-711c67e40b47",
			Type:        settingsmsg.Bundle_TYPE_ROLE,
			DisplayName: "test role - reads | update",
			Name:        "TEST_ROLE",
			Extension:   "opencloud-settings",
			Resource: &settingsmsg.Resource{
				Type: settingsmsg.Resource_TYPE_BUNDLE,
			},
			Settings: []*settingsmsg.Setting{
				{
					Id:   "updateID",
					Name: "update",
					Value: &settingsmsg.Setting_PermissionValue{
						PermissionValue: &settingsmsg.Permission{
							Operation: settingsmsg.Permission_OPERATION_UPDATE,
						},
					},
					Resource: &settingsmsg.Resource{
						Type: settingsmsg.Resource_TYPE_SETTING,
					},
				},
				{
					Id:   "readID",
					Name: "read",
					Value: &settingsmsg.Setting_PermissionValue{
						PermissionValue: &settingsmsg.Permission{
							Operation: settingsmsg.Permission_OPERATION_READ,
						},
					},
					Resource: &settingsmsg.Resource{
						Type: settingsmsg.Resource_TYPE_BUNDLE,
					},
				},
			},
		},
		{
			Id:          "44f1a664-0a7f-461a-b0be-5b59e46bbc7a",
			Type:        settingsmsg.Bundle_TYPE_ROLE,
			DisplayName: "another",
			Name:        "ANOTHER_TEST_ROLE",
			Extension:   "opencloud-settings",
			Resource: &settingsmsg.Resource{
				Type: settingsmsg.Resource_TYPE_BUNDLE,
			},
			Settings: []*settingsmsg.Setting{
				{
					Id:   "readID",
					Name: "read",
					Value: &settingsmsg.Setting_PermissionValue{
						PermissionValue: &settingsmsg.Permission{
							Operation: settingsmsg.Permission_OPERATION_READ,
						},
					},
				},
			},
		},
	}
)

func initStore() *Store {
	s := &Store{
		Logger: logger,
		l:      &sync.Mutex{},
		cfg:    defaults.DefaultConfig(),
	}
	s.cfg.Commons = &shared.Commons{
		AdminUserID: uuid.NewString(),
	}

	_ = NewMDC(s)
	return s
}

func setupRoles(s *Store) {
	for i := range bundles {
		if _, err := s.WriteBundle(bundles[i]); err != nil {
			log.Fatal("error initializing ", err)
		}
	}
}

var (
	roleA = bundles[0].Id // "f36db5e6-..." TEST_ROLE
	roleB = bundles[1].Id // "44f1a664-..." ANOTHER_TEST_ROLE

	// alanPrefixed shares alan's id as a prefix, to catch path matching without a "/" boundary.
	alanPrefixed = alan + "0"
)

type roleWrite struct {
	userID string
	roleID string
}

// requireSingleAssignment checks the "Each user can have exactly one role" contract
// (assignments.go:122) both through the API and directly in storage.
func requireSingleAssignment(t *testing.T, s *Store, userID, roleID string) *settingsmsg.UserRoleAssignment {
	t.Helper()
	list, err := s.ListRoleAssignments(userID)
	require.NoError(t, err)
	require.Len(t, list, 1, "user %s must have exactly one role assignment", userID)
	require.Equal(t, roleID, list[0].RoleId)
	require.Equal(t, userID, list[0].AccountUuid)

	m := mockOf(t, s)
	files := m.Files(accountPath(userID))
	require.Equal(t, []string{assignmentPath(userID, list[0].Id)}, files,
		"storage must hold exactly one assignment file for %s", userID)
	return list[0]
}

func TestAssignmentUniqueness(t *testing.T) {
	var scenarios = []struct {
		name   string
		writes []roleWrite
		want   map[string]string // userID -> the one role they must end up with
	}{
		{
			name:   "roles assignments",
			writes: []roleWrite{{alan, roleA}, {alan, roleB}},
			want:   map[string]string{alan: roleB},
		},
		{
			name:   "same role twice",
			writes: []roleWrite{{alan, roleA}, {alan, roleA}},
			want:   map[string]string{alan: roleA},
		},
		{
			name:   "three writes in a row",
			writes: []roleWrite{{alan, roleA}, {alan, roleB}, {alan, roleA}},
			want:   map[string]string{alan: roleA},
		},
		{
			name:   "two users do not affect each other",
			writes: []roleWrite{{alan, roleA}, {mary, roleB}, {alan, roleB}, {mary, roleA}},
			want:   map[string]string{alan: roleB, mary: roleA},
		},
		{
			name:   "account ids sharing a prefix do not affect each other",
			writes: []roleWrite{{alan, roleA}, {alanPrefixed, roleB}, {alan, roleB}},
			want:   map[string]string{alan: roleB, alanPrefixed: roleB},
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			s := initStore()
			setupRoles(s)

			last := map[string]*settingsmsg.UserRoleAssignment{}
			seenIDs := map[string]bool{}
			for _, w := range scenario.writes {
				a, err := s.WriteRoleAssignment(w.userID, w.roleID)
				require.NoError(t, err)
				require.Equal(t, w.roleID, a.RoleId)
				require.Equal(t, w.userID, a.AccountUuid)
				require.NotEmpty(t, a.Id)
				// F2: every write produces a new assignment id
				require.False(t, seenIDs[a.Id], "assignment id %s was reused", a.Id)
				seenIDs[a.Id] = true
				last[w.userID] = a
			}

			for userID, roleID := range scenario.want {
				got := requireSingleAssignment(t, s, userID, roleID)
				// the surviving assignment is the one returned by the last write
				require.Equal(t, last[userID].Id, got.Id)
			}
		})
	}
}

func TestListRoleAssignmentByRole(t *testing.T) {
	var scenarios = []struct {
		name         string
		assignments  []roleWrite
		queryRole    string
		wantAccounts []string
	}{
		{
			name:         "just 2 assignments",
			assignments:  []roleWrite{{alan, role1}, {mary, role1}},
			queryRole:    role1,
			wantAccounts: []string{alan, mary},
		},
		{
			name:         "no assignments match",
			assignments:  []roleWrite{{alan, role1}, {mary, role1}},
			queryRole:    role2,
			wantAccounts: []string{},
		},
		{
			// F5: exactly the matching accounts, no more, no fewer
			name:         "by-role completeness",
			assignments:  []roleWrite{{alan, role1}, {mary, role2}, {dennis, role1}},
			queryRole:    role1,
			wantAccounts: []string{alan, dennis},
		},
		{
			name:         "only one assignment matches",
			assignments:  []roleWrite{{alan, role1}, {mary, role1}, {dennis, role2}},
			queryRole:    role2,
			wantAccounts: []string{dennis},
		},
		{
			name:         "reassigned user no longer matches the old role",
			assignments:  []roleWrite{{alan, role1}, {mary, role1}, {alan, role2}},
			queryRole:    role1,
			wantAccounts: []string{mary},
		},
		{
			name:         "reassigned user matches the new role",
			assignments:  []roleWrite{{alan, role1}, {mary, role1}, {alan, role2}},
			queryRole:    role2,
			wantAccounts: []string{alan},
		},
		{
			name:         "account ids sharing a prefix",
			assignments:  []roleWrite{{alan, role1}, {alanPrefixed, role2}},
			queryRole:    role1,
			wantAccounts: []string{alan},
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			s := initStore()
			setupRoles(s)
			for _, a := range scenario.assignments {
				ass, err := s.WriteRoleAssignment(a.userID, a.roleID)
				require.NoError(t, err)
				require.Equal(t, ass.RoleId, a.roleID)
			}

			list, err := s.ListRoleAssignmentsByRole(scenario.queryRole)
			require.NoError(t, err)
			gotAccounts := make([]string, 0, len(list))
			for _, ass := range list {
				require.Equal(t, scenario.queryRole, ass.RoleId)
				gotAccounts = append(gotAccounts, ass.AccountUuid)
			}
			require.ElementsMatch(t, scenario.wantAccounts, gotAccounts)
		})
	}
}

func TestDeleteAssignment(t *testing.T) {
	var scenarios = []struct {
		name       string
		userID     string
		firstRole  string
		secondRole string
		// bystander gets an assignment too; removing userID's must leave it intact
		bystander string
	}{
		{
			name:       "roles assignments",
			userID:     alan,
			firstRole:  "f36db5e6-a03c-40df-8413-711c67e40b47",
			secondRole: "44f1a664-0a7f-461a-b0be-5b59e46bbc7a",
		},
		{
			name:       "removing alan's assignment leaves mary's intact",
			userID:     alan,
			firstRole:  roleA,
			secondRole: roleB,
			bystander:  mary,
		},
		{
			name:       "removing alan's assignment leaves a prefix-sharing account intact",
			userID:     alan,
			firstRole:  roleA,
			secondRole: roleB,
			bystander:  alanPrefixed,
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			s := initStore()
			setupRoles(s)
			m := mockOf(t, s)

			var bystanderAssignment *settingsmsg.UserRoleAssignment
			if scenario.bystander != "" {
				var err error
				bystanderAssignment, err = s.WriteRoleAssignment(scenario.bystander, scenario.secondRole)
				require.NoError(t, err)
			}

			assignment, err := s.WriteRoleAssignment(scenario.userID, scenario.firstRole)
			require.NoError(t, err)
			require.Equal(t, assignment.RoleId, scenario.firstRole)
			require.True(t, m.IDExists(assignmentPath(scenario.userID, assignment.Id)))

			list, err := s.ListRoleAssignments(scenario.userID)
			require.NoError(t, err)
			require.Equal(t, 1, len(list))
			require.Equal(t, assignment.Id, list[0].Id)

			err = s.RemoveRoleAssignment(assignment.Id)
			require.NoError(t, err)
			require.False(t, m.IDExists(assignmentPath(scenario.userID, assignment.Id)))

			list, err = s.ListRoleAssignments(scenario.userID)
			require.NoError(t, err)
			require.Equal(t, 0, len(list))

			err = s.RemoveRoleAssignment(assignment.Id)
			require.ErrorIs(t, err, settings.ErrNotFound)

			if bystanderAssignment != nil {
				got := requireSingleAssignment(t, s, scenario.bystander, scenario.secondRole)
				require.Equal(t, bystanderAssignment.Id, got.Id)
			}
		})
	}
}
