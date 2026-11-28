package svc

// Pruebas de componente - Entrega 1, Software Quality 2026-II.
// Componentes: CheckPermission (TC-CP-xx) y AssignRoleToUser (TC-AR-xx). Cinco casos.
// El ID de cada caso (ver documento, seccion 5) es el prefijo del nombre de cada subtest.
//
// Aislamiento: el unico colaborador externo es settings.Manager, sustituido por
// mocks.Manager (stub/spy/mock) o por un fake en memoria. No hay reloj ni aleatoriedad
// (esta suite no usa aleatoriedad).

import (
	"context"
	"errors"
	"fmt"
	"testing"

	grouppb "github.com/cs3org/go-cs3apis/cs3/identity/group/v1beta1"
	userpb "github.com/cs3org/go-cs3apis/cs3/identity/user/v1beta1"
	cs3permissions "github.com/cs3org/go-cs3apis/cs3/permissions/v1beta1"
	rpcv1beta1 "github.com/cs3org/go-cs3apis/cs3/rpc/v1beta1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	merrors "go-micro.dev/v4/errors"
	"go-micro.dev/v4/metadata"

	"github.com/opencloud-eu/opencloud/pkg/middleware"
	settingsmsg "github.com/opencloud-eu/opencloud/protogen/gen/opencloud/messages/settings/v0"
	settingssvc "github.com/opencloud-eu/opencloud/protogen/gen/opencloud/services/settings/v0"
	"github.com/opencloud-eu/opencloud/services/settings/pkg/settings"
	"github.com/opencloud-eu/opencloud/services/settings/pkg/settings/mocks"
	"github.com/opencloud-eu/opencloud/services/settings/pkg/store/defaults"
)

const (
	p1Self  = "61445573-4dbe-4d56-88dc-88ab47aceba7"
	p1Other = "9a1b2c3d-0000-4000-8000-000000000002"
	p1Perm  = "Drives.Create.all"
)

// ---------------------------------------------------------------- helpers

func p1Ctx(account string) context.Context {
	return metadata.Set(context.Background(), middleware.AccountID, account)
}

// p1Manager devuelve un Manager mock ligado al test: una llamada no prevista falla el test.
func p1Manager(t *testing.T) *mocks.Manager {
	m := &mocks.Manager{}
	m.Test(t)
	return m
}

func p1Svc(m settings.Manager) Service {
	return Service{id: "opencloud-settings-p1", manager: m}
}

func p1Assignments(account string, roleIDs ...string) []*settingsmsg.UserRoleAssignment {
	out := make([]*settingsmsg.UserRoleAssignment, 0, len(roleIDs))
	for i, r := range roleIDs {
		out = append(out, &settingsmsg.UserRoleAssignment{
			Id:          fmt.Sprintf("assignment-%d", i),
			AccountUuid: account,
			RoleId:      r,
		})
	}
	return out
}

func p1GrantingPermission() *settingsmsg.Permission {
	return &settingsmsg.Permission{
		Operation:  settingsmsg.Permission_OPERATION_READWRITE,
		Constraint: settingsmsg.Permission_CONSTRAINT_ALL,
	}
}

// p1StubWriteEcho registra WriteRoleAssignment como stub que devuelve la asignacion pedida.
// Se marca opcional (Maybe) para poder afirmar despues si se llamo o no (spy).
func p1StubWriteEcho(m *mocks.Manager) {
	m.On("WriteRoleAssignment", mock.Anything, mock.Anything).Maybe().Return(
		func(account, role string) (*settingsmsg.UserRoleAssignment, error) {
			return &settingsmsg.UserRoleAssignment{Id: "new-assignment", AccountUuid: account, RoleId: role}, nil
		}, nil)
}

// p1CanManageRoles configura al usuario del contexto con permiso de gestion de roles.
func p1CanManageRoles(m *mocks.Manager, ctxUser string) {
	m.On("ListRoleAssignments", ctxUser).Maybe().Return(p1Assignments(ctxUser, defaults.BundleUUIDRoleAdmin), nil)
	m.On("ReadPermissionByID", RoleManagementPermissionID, mock.Anything).Maybe().Return(p1GrantingPermission(), nil)
}

// p1CannotManageRoles configura al usuario del contexto sin permiso de gestion de roles.
func p1CannotManageRoles(m *mocks.Manager, ctxUser string) {
	m.On("ListRoleAssignments", ctxUser).Maybe().Return(p1Assignments(ctxUser, defaults.BundleUUIDRoleUser), nil)
	m.On("ReadPermissionByID", RoleManagementPermissionID, mock.Anything).Maybe().Return(nil, errors.New("permission not found"))
}

func p1Assign(s Service, ctx context.Context, account, role string) (*settingssvc.AssignRoleToUserResponse, error) {
	res := &settingssvc.AssignRoleToUserResponse{}
	err := s.AssignRoleToUser(ctx, &settingssvc.AssignRoleToUserRequest{AccountUuid: account, RoleId: role}, res)
	return res, err
}

func p1RequireMicroError(t *testing.T, err error, code int32) {
	t.Helper()
	require.Error(t, err)
	var me *merrors.Error
	require.True(t, errors.As(err, &me), "se esperaba un error de go-micro, se obtuvo %T: %v", err, err)
	assert.EqualValues(t, code, me.Code, "codigo de error: %v", err)
}

func p1WriteCalls(m *mocks.Manager) int {
	n := 0
	for _, c := range m.Calls {
		if c.Method == "WriteRoleAssignment" {
			n++
		}
	}
	return n
}

func p1UserReq(id, perm string) *cs3permissions.CheckPermissionRequest {
	return &cs3permissions.CheckPermissionRequest{
		Permission: perm,
		SubjectRef: &cs3permissions.SubjectReference{
			Spec: &cs3permissions.SubjectReference_UserId{UserId: &userpb.UserId{OpaqueId: id}},
		},
	}
}

func p1GroupReq(id, perm string) *cs3permissions.CheckPermissionRequest {
	return &cs3permissions.CheckPermissionRequest{
		Permission: perm,
		SubjectRef: &cs3permissions.SubjectReference{
			Spec: &cs3permissions.SubjectReference_GroupId{GroupId: &grouppb.GroupId{OpaqueId: id}},
		},
	}
}

func p1RequireStatus(t *testing.T, resp *cs3permissions.CheckPermissionResponse, err error, code rpcv1beta1.Code) {
	t.Helper()
	require.NoError(t, err, "CheckPermission expresa los fallos en Status, no en error (I2)")
	require.NotNil(t, resp)
	require.NotNil(t, resp.GetStatus())
	assert.Equal(t, code, resp.GetStatus().GetCode(), "mensaje: %s", resp.GetStatus().GetMessage())
}

// p1FakeRoleStore es un fake en memoria del Manager: una asignacion por usuario,
// como el almacen real (WriteRoleAssignment reemplaza la asignacion previa).
type p1FakeRoleStore struct {
	*mocks.Manager
	byAccount map[string][]*settingsmsg.UserRoleAssignment
	writes    int
}

func newP1FakeRoleStore(t *testing.T) *p1FakeRoleStore {
	return &p1FakeRoleStore{Manager: p1Manager(t), byAccount: map[string][]*settingsmsg.UserRoleAssignment{}}
}

func (f *p1FakeRoleStore) ListRoleAssignments(account string) ([]*settingsmsg.UserRoleAssignment, error) {
	return append([]*settingsmsg.UserRoleAssignment{}, f.byAccount[account]...), nil
}

func (f *p1FakeRoleStore) WriteRoleAssignment(account, role string) (*settingsmsg.UserRoleAssignment, error) {
	f.writes++
	a := &settingsmsg.UserRoleAssignment{Id: fmt.Sprintf("a-%d", f.writes), AccountUuid: account, RoleId: role}
	f.byAccount[account] = []*settingsmsg.UserRoleAssignment{a}
	return a, nil
}

func (f *p1FakeRoleStore) ReadPermissionByID(string, []string) (*settingsmsg.Permission, error) {
	return nil, errors.New("fake: sin permisos de gestion")
}

// ---------------------------------------------------------------- CheckPermission

func TestP1_CheckPermission(t *testing.T) {
	ctx := context.Background()

	t.Run("TC-CP-01 ningun rol otorga el permiso (ErrNotFound)", func(t *testing.T) {
		m := p1Manager(t)
		m.On("ListRoleAssignments", "user-x").Return(p1Assignments("user-x", "role-a"), nil)
		m.On("ReadPermissionByName", p1Perm, []string{"role-a"}).Return(nil, settings.ErrNotFound)

		resp, err := p1Svc(m).CheckPermission(ctx, p1UserReq("user-x", p1Perm))

		p1RequireStatus(t, resp, err, rpcv1beta1.Code_CODE_PERMISSION_DENIED)
	})

	t.Run("TC-CP-02 SubjectRef nulo: no se concede por un identificador vacio", func(t *testing.T) {
		// El Manager estaria dispuesto a conceder para cualquier cuenta. El contrato (P1)
		// exige un sujeto; sin el, el componente no debe consultar ni conceder.
		m := p1Manager(t)
		m.On("ListRoleAssignments", mock.Anything).Maybe().Return(p1Assignments("", "role-a"), nil)
		m.On("ReadPermissionByName", p1Perm, mock.Anything).Maybe().Return(p1GrantingPermission(), nil)

		resp, err := p1Svc(m).CheckPermission(ctx, &cs3permissions.CheckPermissionRequest{Permission: p1Perm})

		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.NotEqual(t, rpcv1beta1.Code_CODE_OK, resp.GetStatus().GetCode(),
			"se concedio un permiso a un sujeto inexistente (identificador vacio)")
		m.AssertNotCalled(t, "ListRoleAssignments", "")
	})

}

// ---------------------------------------------------------------- AssignRoleToUser

func TestP1_AssignRoleToUser(t *testing.T) {
	const (
		roleUser      = defaults.BundleUUIDRoleUser
		roleUserLight = defaults.BundleUUIDRoleUserLight
		roleAdmin     = defaults.BundleUUIDRoleAdmin
	)
	_ = roleAdmin

	t.Run("TC-AR-01 secuencia: sin rol -> autoasigna User -> intenta UserLight", func(t *testing.T) {
		fake := newP1FakeRoleStore(t)
		s := p1Svc(fake)

		_, err := p1Assign(s, p1Ctx(p1Self), p1Self, roleUser)
		require.NoError(t, err, "primera autoasignacion")

		_, err = p1Assign(s, p1Ctx(p1Self), p1Self, roleUserLight)
		p1RequireMicroError(t, err, 403)

		assert.Equal(t, 1, fake.writes, "una sola escritura")
		require.Len(t, fake.byAccount[p1Self], 1)
		assert.Equal(t, roleUser, fake.byAccount[p1Self][0].GetRoleId(), "el rol no cambio")
	})

	t.Run("TC-AR-02 autoasignacion cuando falla la lectura de asignaciones (fallo seguro)", func(t *testing.T) {
		m := p1Manager(t)
		m.On("ListRoleAssignments", p1Self).Return(nil, errors.New("storage down"))
		p1StubWriteEcho(m)

		_, err := p1Assign(p1Svc(m), p1Ctx(p1Self), p1Self, roleUser)

		assert.Error(t, err, "I1: si no se pudo comprobar que ya hay un rol, no se debe conceder")
		assert.Zero(t, p1WriteCalls(m), "se escribio una asignacion sin poder verificar las existentes")
	})

	t.Run("TC-AR-03 RoleId vacio", func(t *testing.T) {
		m := p1Manager(t)
		p1CanManageRoles(m, p1Self)
		p1StubWriteEcho(m)

		_, err := p1Assign(p1Svc(m), p1Ctx(p1Self), p1Other, "")

		p1RequireMicroError(t, err, 400)
		assert.Zero(t, p1WriteCalls(m), "P2: se escribio una asignacion con RoleId vacio")
	})

}
