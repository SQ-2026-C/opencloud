package svc

import (
	"context"
	"errors"
	"fmt"
	"testing"

	grouppb "github.com/cs3org/go-cs3apis/cs3/identity/group/v1beta1"
	userpb "github.com/cs3org/go-cs3apis/cs3/identity/user/v1beta1"
	cs3permissions "github.com/cs3org/go-cs3apis/cs3/permissions/v1beta1"
	rpcv1beta1 "github.com/cs3org/go-cs3apis/cs3/rpc/v1beta1"
	settingsmsg "github.com/opencloud-eu/opencloud/protogen/gen/opencloud/messages/settings/v0"
	"github.com/opencloud-eu/opencloud/services/settings/pkg/settings"
	"github.com/opencloud-eu/opencloud/services/settings/pkg/settings/mocks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// CheckPermission (service.go:58) is the CS3 entry point other services use to
// ask "may subject X do permission P?". The answer is always in the response
// Status, never a gRPC error.
func TestCheckPermission(t *testing.T) {
	const (
		account    = "61445573-4dbe-4d56-88dc-88ab47aceba7"
		group      = "a1b2c3d4-0000-4000-8000-000000000001"
		roleID     = "aceb15b8-7486-479f-ae32-c91118e07a39"
		permission = "Settings.ReadWrite"
	)
	userSubject := &cs3permissions.SubjectReference{
		Spec: &cs3permissions.SubjectReference_UserId{UserId: &userpb.UserId{OpaqueId: account}},
	}
	groupSubject := &cs3permissions.SubjectReference{
		Spec: &cs3permissions.SubjectReference_GroupId{GroupId: &grouppb.GroupId{OpaqueId: group}},
	}
	assigned := []*settingsmsg.UserRoleAssignment{{AccountUuid: account, RoleId: roleID}}
	granted := &settingsmsg.Permission{
		Operation:  settingsmsg.Permission_OPERATION_READWRITE,
		Constraint: settingsmsg.Permission_CONSTRAINT_ALL,
	}

	scenarios := []struct {
		name    string
		subject *cs3permissions.SubjectReference
		setup   func(m *mocks.Manager)
		want    rpcv1beta1.Code
		verify  func(t *testing.T, m *mocks.Manager)
	}{
		{
			name:    "allowed when one of the user's roles has the permission",
			subject: userSubject,
			setup: func(m *mocks.Manager) {
				m.On("ListRoleAssignments", account).Return(assigned, nil)
				m.On("ReadPermissionByName", permission, []string{roleID}).Return(granted, nil)
			},
			want: rpcv1beta1.Code_CODE_OK,
		},
		{
			name:    "denied when no role has the permission",
			subject: userSubject,
			setup: func(m *mocks.Manager) {
				m.On("ListRoleAssignments", account).Return(assigned, nil)
				m.On("ReadPermissionByName", permission, []string{roleID}).
					Return(nil, fmt.Errorf("wrapped: %w", settings.ErrNotFound))
			},
			want: rpcv1beta1.Code_CODE_PERMISSION_DENIED,
		},
		{
			name:    "denied when the user has no role at all",
			subject: userSubject,
			setup: func(m *mocks.Manager) {
				m.On("ListRoleAssignments", account).Return([]*settingsmsg.UserRoleAssignment{}, nil)
				m.On("ReadPermissionByName", permission, []string{}).Return(nil, settings.ErrNotFound)
			},
			want: rpcv1beta1.Code_CODE_PERMISSION_DENIED,
		},
		{
			name:    "internal when listing assignments fails",
			subject: userSubject,
			setup: func(m *mocks.Manager) {
				m.On("ListRoleAssignments", account).Return(nil, errors.New("storage down"))
			},
			want: rpcv1beta1.Code_CODE_INTERNAL,
			verify: func(t *testing.T, m *mocks.Manager) {
				m.AssertNotCalled(t, "ReadPermissionByName", mock.Anything, mock.Anything)
			},
		},
		{
			name:    "internal when reading the permission fails with a non-not-found error",
			subject: userSubject,
			setup: func(m *mocks.Manager) {
				m.On("ListRoleAssignments", account).Return(assigned, nil)
				m.On("ReadPermissionByName", permission, []string{roleID}).Return(nil, errors.New("storage down"))
			},
			want: rpcv1beta1.Code_CODE_INTERNAL,
		},
		{
			// Quirk: a group subject's id is looked up as if it were an account
			// id, so a group only "has" a permission if an account with the same
			// id has a role assignment.
			name:    "group subject id is looked up as an account id",
			subject: groupSubject,
			setup: func(m *mocks.Manager) {
				m.On("ListRoleAssignments", group).Return([]*settingsmsg.UserRoleAssignment{}, nil)
				m.On("ReadPermissionByName", permission, []string{}).Return(nil, settings.ErrNotFound)
			},
			want: rpcv1beta1.Code_CODE_PERMISSION_DENIED,
			verify: func(t *testing.T, m *mocks.Manager) {
				m.AssertCalled(t, "ListRoleAssignments", group)
			},
		},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			manager := &mocks.Manager{}
			sc.setup(manager)
			svc := Service{manager: manager}

			res, err := svc.CheckPermission(context.Background(), &cs3permissions.CheckPermissionRequest{
				Permission: permission,
				SubjectRef: sc.subject,
			})

			require.NoError(t, err, "CheckPermission reports failures in the Status, not as an error")
			require.NotNil(t, res.GetStatus())
			assert.Equal(t, sc.want, res.GetStatus().GetCode())
			manager.AssertExpectations(t)
			if sc.verify != nil {
				sc.verify(t, manager)
			}
		})
	}
}
