package store

import (
	"testing"

	settingsmsg "github.com/opencloud-eu/opencloud/protogen/gen/opencloud/messages/settings/v0"
	"github.com/opencloud-eu/opencloud/services/settings/pkg/settings"
	"github.com/opencloud-eu/opencloud/services/settings/pkg/util"
	"github.com/stretchr/testify/require"
)

const (
	// roleA is bundles[0] (TEST_ROLE): "update" on TYPE_SETTING and "read" on TYPE_BUNDLE, both with resource id "".
	permRoleCreator  = "5a1d5b0e-0000-4000-8000-00000000000c" // "create" on TYPE_BUNDLE, resource id ""
	permRoleWildcard = "5a1d5b0e-0000-4000-8000-00000000000a" // "manage-users" on TYPE_USER, resource id "all"
	permRoleUnknown  = "5a1d5b0e-0000-4000-8000-0000000000ff" // never written to storage
	permRoleUserAll  = "5a1d5b0e-0000-4000-8000-000000000071" // "read-users" on USER/all
	permRoleMixed    = "5a1d5b0e-0000-4000-8000-000000000072" // "update-user-x" on USER/x, "delete-bundle-x" on BUNDLE/x
)

func permissionSetting(id string, resType settingsmsg.Resource_Type, resID string, op settingsmsg.Permission_Operation) *settingsmsg.Setting {
	return &settingsmsg.Setting{
		Id:       id,
		Name:     id,
		Resource: &settingsmsg.Resource{Type: resType, Id: resID},
		Value: &settingsmsg.Setting_PermissionValue{PermissionValue: &settingsmsg.Permission{
			Operation: op,
		}},
	}
}

var permissionBundles = []*settingsmsg.Bundle{
	{
		Id:   permRoleUserAll,
		Type: settingsmsg.Bundle_TYPE_ROLE,
		Name: "USER_READER",
		Settings: []*settingsmsg.Setting{
			permissionSetting("read-users", settingsmsg.Resource_TYPE_USER, util.ResourceIDAll, settingsmsg.Permission_OPERATION_READ),
		},
	},
	{
		Id:   permRoleMixed,
		Type: settingsmsg.Bundle_TYPE_ROLE,
		Name: "MIXED",
		Settings: []*settingsmsg.Setting{
			permissionSetting("update-user-x", settingsmsg.Resource_TYPE_USER, "x", settingsmsg.Permission_OPERATION_UPDATE),
			permissionSetting("delete-bundle-x", settingsmsg.Resource_TYPE_BUNDLE, "x", settingsmsg.Permission_OPERATION_DELETE),
		},
	},
	{
		Id:   permRoleCreator,
		Type: settingsmsg.Bundle_TYPE_ROLE,
		Name: "CREATOR",
		Settings: []*settingsmsg.Setting{
			{
				Id:       "createID",
				Name:     "create",
				Resource: &settingsmsg.Resource{Type: settingsmsg.Resource_TYPE_BUNDLE},
				Value: &settingsmsg.Setting_PermissionValue{PermissionValue: &settingsmsg.Permission{
					Operation: settingsmsg.Permission_OPERATION_CREATE,
				}},
			},
		},
	},
	{
		Id:   permRoleWildcard,
		Type: settingsmsg.Bundle_TYPE_ROLE,
		Name: "USER_MANAGER",
		Settings: []*settingsmsg.Setting{
			{
				Id:       "manageUsersID",
				Name:     "manage-users",
				Resource: &settingsmsg.Resource{Type: settingsmsg.Resource_TYPE_USER, Id: util.ResourceIDAll},
				Value: &settingsmsg.Setting_PermissionValue{PermissionValue: &settingsmsg.Permission{
					Operation:  settingsmsg.Permission_OPERATION_READWRITE,
					Constraint: settingsmsg.Permission_CONSTRAINT_ALL,
				}},
			},
		},
	},
}

func setupPermissionRoles(t *testing.T, s *Store) {
	t.Helper()
	setupRoles(s)
	for _, b := range permissionBundles {
		_, err := s.WriteBundle(b)
		require.NoError(t, err)
	}
}

func operations(ps []*settingsmsg.Permission) []settingsmsg.Permission_Operation {
	out := make([]settingsmsg.Permission_Operation, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Operation)
	}
	return out
}

func TestPermission(t *testing.T) {
	type method int
	const (
		byID method = iota
		byName
		byResource
	)

	var scenarios = []struct {
		name     string
		method   method
		key      string                // permission id or name
		resource *settingsmsg.Resource // for byResource
		roleIDs  []string
		// expectations
		wantOps     []settingsmsg.Permission_Operation // byResource, or the single op for byID/byName
		wantNil     bool                               // byID/byName: no permission returned
		wantErrorIs error
	}{
		// the original happy paths
		{name: "by id", method: byID, key: "readID", roleIDs: []string{roleA},
			wantOps: []settingsmsg.Permission_Operation{settingsmsg.Permission_OPERATION_READ}},
		{name: "by name", method: byName, key: "read", roleIDs: []string{roleA},
			wantOps: []settingsmsg.Permission_Operation{settingsmsg.Permission_OPERATION_READ}},
		{name: "by resource", method: byResource, resource: &settingsmsg.Resource{Type: settingsmsg.Resource_TYPE_BUNDLE}, roleIDs: []string{roleA},
			wantOps: []settingsmsg.Permission_Operation{settingsmsg.Permission_OPERATION_READ}},

		// not found: the three methods do not agree on how to report it
		{name: "name that does not exist returns ErrNotFound", method: byName, key: "does-not-exist", roleIDs: []string{roleA},
			wantNil: true, wantErrorIs: settings.ErrNotFound},
		{name: "id that does not exist returns nil, nil", method: byID, key: "does-not-exist", roleIDs: []string{roleA},
			wantNil: true},
		{name: "by name with no roles returns ErrNotFound", method: byName, key: "read", roleIDs: nil,
			wantNil: true, wantErrorIs: settings.ErrNotFound},
		{name: "by resource with no roles returns empty", method: byResource, resource: &settingsmsg.Resource{Type: settingsmsg.Resource_TYPE_BUNDLE}, roleIDs: nil,
			wantOps: []settingsmsg.Permission_Operation{}},

		// unknown roles are skipped, not fatal
		{name: "unknown role id is skipped by name", method: byName, key: "read", roleIDs: []string{permRoleUnknown, roleA},
			wantOps: []settingsmsg.Permission_Operation{settingsmsg.Permission_OPERATION_READ}},
		{name: "unknown role id is skipped by id", method: byID, key: "readID", roleIDs: []string{permRoleUnknown, roleA},
			wantOps: []settingsmsg.Permission_Operation{settingsmsg.Permission_OPERATION_READ}},
		{name: "only unknown role id", method: byID, key: "readID", roleIDs: []string{permRoleUnknown},
			wantNil: true},
		{name: "unknown role id is skipped by resource", method: byResource, resource: &settingsmsg.Resource{Type: settingsmsg.Resource_TYPE_BUNDLE}, roleIDs: []string{permRoleUnknown, roleA},
			wantOps: []settingsmsg.Permission_Operation{settingsmsg.Permission_OPERATION_READ}},

		// several roles
		{name: "two roles give the union by resource", method: byResource, resource: &settingsmsg.Resource{Type: settingsmsg.Resource_TYPE_BUNDLE}, roleIDs: []string{roleA, permRoleCreator},
			wantOps: []settingsmsg.Permission_Operation{settingsmsg.Permission_OPERATION_READ, settingsmsg.Permission_OPERATION_CREATE}},
		// F7: union across roles, filtered by type and id
		{name: "union of roles for USER/x excludes the BUNDLE/x permission", method: byResource, resource: &settingsmsg.Resource{Type: settingsmsg.Resource_TYPE_USER, Id: "x"}, roleIDs: []string{permRoleUserAll, permRoleMixed},
			wantOps: []settingsmsg.Permission_Operation{settingsmsg.Permission_OPERATION_READ, settingsmsg.Permission_OPERATION_UPDATE}},
		{name: "permission only in the second role is found by name", method: byName, key: "create", roleIDs: []string{roleA, permRoleCreator},
			wantOps: []settingsmsg.Permission_Operation{settingsmsg.Permission_OPERATION_CREATE}},

		// resource matching (util.IsResourceMatched)
		{name: "wildcard resource id matches any id", method: byResource, resource: &settingsmsg.Resource{Type: settingsmsg.Resource_TYPE_USER, Id: "some-user"}, roleIDs: []string{permRoleWildcard},
			wantOps: []settingsmsg.Permission_Operation{settingsmsg.Permission_OPERATION_READWRITE}},
		{name: "wildcard resource id matches an empty id", method: byResource, resource: &settingsmsg.Resource{Type: settingsmsg.Resource_TYPE_USER}, roleIDs: []string{permRoleWildcard},
			wantOps: []settingsmsg.Permission_Operation{settingsmsg.Permission_OPERATION_READWRITE}},
		{name: "type mismatch matches nothing", method: byResource, resource: &settingsmsg.Resource{Type: settingsmsg.Resource_TYPE_SYSTEM}, roleIDs: []string{roleA, permRoleWildcard},
			wantOps: []settingsmsg.Permission_Operation{}},
		{name: "type matches setting but not bundle permission", method: byResource, resource: &settingsmsg.Resource{Type: settingsmsg.Resource_TYPE_SETTING}, roleIDs: []string{roleA},
			wantOps: []settingsmsg.Permission_Operation{settingsmsg.Permission_OPERATION_UPDATE}},
		// The happy path above only passes because both resource ids are "". A
		// permission defined for resource id "" does not match a concrete id.
		{name: "permission for resource id \"\" does not match a concrete id", method: byResource, resource: &settingsmsg.Resource{Type: settingsmsg.Resource_TYPE_BUNDLE, Id: "x"}, roleIDs: []string{roleA},
			wantOps: []settingsmsg.Permission_Operation{}},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			s := initStore()
			setupPermissionRoles(t, s)

			switch sc.method {
			case byID, byName:
				var p *settingsmsg.Permission
				var err error
				if sc.method == byID {
					p, err = s.ReadPermissionByID(sc.key, sc.roleIDs)
				} else {
					p, err = s.ReadPermissionByName(sc.key, sc.roleIDs)
				}
				if sc.wantErrorIs != nil {
					require.ErrorIs(t, err, sc.wantErrorIs)
				} else {
					require.NoError(t, err)
				}
				if sc.wantNil {
					require.Nil(t, p)
					return
				}
				require.NotNil(t, p)
				require.Equal(t, sc.wantOps[0], p.Operation)

			case byResource:
				ps, err := s.ListPermissionsByResource(sc.resource, sc.roleIDs)
				require.NoError(t, err)
				require.ElementsMatch(t, sc.wantOps, operations(ps))
			}
		})
	}
}
