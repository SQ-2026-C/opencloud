package store

// Shared fixtures for the bundle and value tests (not part of the
// role-assignment and permission store suite).

import (
	"log"
	"sync"

	"github.com/google/uuid"
	olog "github.com/opencloud-eu/opencloud/pkg/log"
	"github.com/opencloud-eu/opencloud/pkg/shared"
	settingsmsg "github.com/opencloud-eu/opencloud/protogen/gen/opencloud/messages/settings/v0"
	"github.com/opencloud-eu/opencloud/services/settings/pkg/config/defaults"
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
