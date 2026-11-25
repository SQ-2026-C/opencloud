package store

import (
	"context"
	"encoding/json"
	"errors"
	"path"
	"sort"
	"strings"
	"sync"
	"testing"

	. "github.com/onsi/gomega"
	settingsmsg "github.com/opencloud-eu/opencloud/protogen/gen/opencloud/messages/settings/v0"
	"github.com/opencloud-eu/opencloud/services/settings/pkg/config/defaults"
	rdefaults "github.com/opencloud-eu/opencloud/services/settings/pkg/store/defaults"
	"github.com/opencloud-eu/reva/v3/pkg/errtypes"
	"github.com/stretchr/testify/require"
)

const (
	// account UUIDs
	accountUUID1 = "c4572da7-6142-4383-8fc6-efde3d463036"
	//accountUUID2 = "e11f9769-416a-427d-9441-41a0e51391d7"
	//accountUUID3 = "633ecd77-1980-412a-8721-bf598a330bb4"

	// extension names
	extension1 = "test-extension-1"
	extension2 = "test-extension-2"

	// bundle ids
	bundle1 = "2f06addf-4fd2-49d5-8f71-00fbd3a3ec47"
	bundle2 = "2d745744-749c-4286-8e92-74a24d8331c5"
	bundle3 = "d8fd27d1-c00b-4794-a658-416b756a72ff"

	// setting ids
	setting1 = "c7ebbc8b-d15a-4f2e-9d7d-d6a4cf858d1a"
	setting2 = "3fd9a3d9-20b7-40d4-9294-b22bb5868c10"
	setting3 = "24bb9535-3df4-42f1-a622-7c0562bec99f"

	// value ids
	value1 = "fd3b6221-dc13-4a22-824d-2480495f1cdb"
	value2 = "2a0bd9b0-ca1d-491a-8c56-d2ddfd68ded8"
	value3 = "b42702d2-5e4d-4d73-b133-e1f9e285355e"
)

// use "unit" or "integration" do define test type. You need a running OpenCloud instance for integration tests
var testtype = "unit"

// MockedMetadataClient is an in-memory stand-in for the CS3 metadata storage
// (reva pkg/storage/utils/metadata/cs3.go). It models a folder tree so it
// behaves like the real client wherever the store depends on it:
//   - SimpleDownload, ReadDir and Delete on a missing path return errtypes.NotFound
//   - ReadDir lists each direct child (file or folder) exactly once
//   - paths match on "/" boundaries, so "accounts/a" never matches "accounts/ab"
//   - MakeDirIfNotExist creates real, possibly empty, folders
//
// Tests can also inspect it (IDExists, Files), spy on it (Calls) and inject
// faults or pauses before any operation (OnCall, FailOn).
type MockedMetadataClient struct {
	mu    sync.Mutex
	data  map[string][]byte   // files, keyed by path
	dirs  map[string]struct{} // folders, keyed by path
	calls []mdcCall
	hooks []mdcHook
}

// mdcCall records one call made to the fake.
type mdcCall struct {
	Op   string
	Path string
}

// mdcHook runs before an operation. A non-nil error is returned to the caller
// and the operation is not performed.
type mdcHook struct {
	op    string
	match func(path string) bool
	fn    func(path string) error
}

func newMockedMDC() *MockedMetadataClient {
	return &MockedMetadataClient{
		data: make(map[string][]byte),
		dirs: make(map[string]struct{}),
	}
}

// NewMDC instantiates a mocked MetadataClient
func NewMDC(s *Store) error {
	var mdc MetadataClient
	switch testtype {
	case "unit":
		mdc = newMockedMDC()
	case "integration":
		mdc = NewMetadataClient(defaults.DefaultConfig().Metadata)
	}
	return s.initMetadataClient(mdc)
}

// mockOf returns the in-memory client behind a store set up with NewMDC.
// Tests that need to inspect storage are skipped in integration mode.
func mockOf(t *testing.T, s *Store) *MockedMetadataClient {
	t.Helper()
	m, ok := s.mdc.(*MockedMetadataClient)
	if !ok {
		t.Skip("test needs the in-memory MockedMetadataClient")
	}
	return m
}

func cleanPath(p string) string { return strings.Trim(p, "/") }

// isUnder reports whether p is strictly inside folder dir.
func isUnder(p, dir string) bool { return strings.HasPrefix(p, dir+"/") }

// OnCall registers fn to run before every op ("SimpleUpload", "ReadDir", ...)
// whose path satisfies match. fn runs without the fake's lock held, so it may block.
func (m *MockedMetadataClient) OnCall(op string, match func(path string) bool, fn func(path string) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hooks = append(m.hooks, mdcHook{op: op, match: match, fn: fn})
}

// FailOn makes op on exactly path return err.
func (m *MockedMetadataClient) FailOn(op, path string, err error) {
	path = cleanPath(path)
	m.OnCall(op, func(p string) bool { return p == path }, func(string) error { return err })
}

// Calls returns every call made so far, in order.
func (m *MockedMetadataClient) Calls() []mdcCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]mdcCall(nil), m.calls...)
}

// ResetCalls forgets the recorded calls.
func (m *MockedMetadataClient) ResetCalls() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = nil
}

func (m *MockedMetadataClient) before(op, path string) error {
	m.mu.Lock()
	m.calls = append(m.calls, mdcCall{Op: op, Path: path})
	var fns []func(string) error
	for _, h := range m.hooks {
		if h.op == op && h.match(path) {
			fns = append(fns, h.fn)
		}
	}
	m.mu.Unlock()
	for _, fn := range fns {
		if err := fn(path); err != nil {
			return err
		}
	}
	return nil
}

// mkdirAll must be called with m.mu held.
func (m *MockedMetadataClient) mkdirAll(p string) {
	for p != "" && p != "." {
		m.dirs[p] = struct{}{}
		p = path.Dir(p)
	}
}

// SimpleDownload returns errtypes.NotFound if not found
func (m *MockedMetadataClient) SimpleDownload(_ context.Context, id string) ([]byte, error) {
	id = cleanPath(id)
	if err := m.before("SimpleDownload", id); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if data, ok := m.data[id]; ok {
		return append([]byte(nil), data...), nil
	}
	return nil, errtypes.NotFound(id)
}

// SimpleUpload writes a file, creating its parent folders.
func (m *MockedMetadataClient) SimpleUpload(_ context.Context, id string, content []byte) error {
	id = cleanPath(id)
	if err := m.before("SimpleUpload", id); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mkdirAll(path.Dir(id))
	m.data[id] = append([]byte(nil), content...)
	return nil
}

// Delete removes a file or a folder with everything in it.
// Like the real client it returns errtypes.NotFound if nothing exists at id.
func (m *MockedMetadataClient) Delete(_ context.Context, id string) error {
	id = cleanPath(id)
	if err := m.before("Delete", id); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	found := false
	for k := range m.data {
		if k == id || isUnder(k, id) {
			delete(m.data, k)
			found = true
		}
	}
	for k := range m.dirs {
		if k == id || isUnder(k, id) {
			delete(m.dirs, k)
			found = true
		}
	}
	if !found {
		return errtypes.NotFound(id)
	}
	return nil
}

// ReadDir lists the direct children of a folder, sorted, each exactly once.
// Like the real client it returns errtypes.NotFound for a missing folder.
func (m *MockedMetadataClient) ReadDir(_ context.Context, id string) ([]string, error) {
	id = cleanPath(id)
	if err := m.before("ReadDir", id); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.dirs[id]; !ok {
		return nil, errtypes.NotFound(id)
	}
	children := map[string]struct{}{}
	collect := func(k string) {
		if isUnder(k, id) {
			children[strings.SplitN(strings.TrimPrefix(k, id+"/"), "/", 2)[0]] = struct{}{}
		}
	}
	for k := range m.data {
		collect(k)
	}
	for k := range m.dirs {
		collect(k)
	}
	out := make([]string, 0, len(children))
	for c := range children {
		out = append(out, c)
	}
	sort.Strings(out)
	return out, nil
}

// MakeDirIfNotExist creates the folder and its parents.
func (m *MockedMetadataClient) MakeDirIfNotExist(_ context.Context, id string) error {
	id = cleanPath(id)
	if err := m.before("MakeDirIfNotExist", id); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mkdirAll(id)
	return nil
}

// Init does nothing unless a hook makes it fail.
func (m *MockedMetadataClient) Init(_ context.Context, id string) error {
	return m.before("Init", id)
}

// IDExists is a helper to check if a file exists at id
func (m *MockedMetadataClient) IDExists(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.data[cleanPath(id)]
	return ok
}

// IDHasContent returns true if the value stored under id has the given content (converted to string)
func (m *MockedMetadataClient) IDHasContent(id string, content []byte) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return string(m.data[cleanPath(id)]) == string(content)
}

// Files returns the paths of the files directly inside folder dir, sorted.
func (m *MockedMetadataClient) Files(dir string) []string {
	dir = cleanPath(dir)
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for k := range m.data {
		if isUnder(k, dir) && !strings.Contains(strings.TrimPrefix(k, dir+"/"), "/") {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// seedAssignment writes an assignment file directly, bypassing the store,
// to set up states the public API would not produce.
func (m *MockedMetadataClient) seedAssignment(t *testing.T, accountUUID, assignmentID, roleID string) {
	t.Helper()
	b, err := json.Marshal(&settingsmsg.UserRoleAssignment{Id: assignmentID, AccountUuid: accountUUID, RoleId: roleID})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SimpleUpload(context.Background(), assignmentPath(accountUUID, assignmentID), b); err != nil {
		t.Fatal(err)
	}
}

// TestAdminUserIDInit test the happy path during initialization
func TestAdminUserIDInit(t *testing.T) {
	RegisterTestingT(t)
	s := &Store{
		cfg: defaults.DefaultConfig(),
	}
	s.cfg.Bundles = rdefaults.GenerateBundlesDefaultRoles()
	s.cfg.AdminUserID = "admin"

	// the first assignment is always happening during the initialisation of the metadata client
	err := NewMDC(s)
	Expect(err).To(BeNil())

	assID, err := s.mdc.ReadDir(context.TODO(), accountPath(s.cfg.AdminUserID))
	Expect(err).To(BeNil())
	Expect(len(assID)).To(Equal(1))
	ass, err := s.mdc.SimpleDownload(context.TODO(), assignmentPath(s.cfg.AdminUserID, assID[0]))
	Expect(err).To(BeNil())
	Expect(ass).ToNot(BeNil())

	assignment := &settingsmsg.UserRoleAssignment{}
	err = json.Unmarshal(ass, assignment)
	Expect(err).To(BeNil())
	Expect(assignment.RoleId).To(Equal(rdefaults.BundleUUIDRoleAdmin))
}

// TestAdminUserIDUpdate test the update on following initialisations
func TestAdminUserIDUpdate(t *testing.T) {
	RegisterTestingT(t)
	s := &Store{
		cfg: defaults.DefaultConfig(),
	}
	s.cfg.Bundles = rdefaults.GenerateBundlesDefaultRoles()
	s.cfg.AdminUserID = "admin"

	// the first assignment is always happening during the initialisation of the metadata client
	err := NewMDC(s)
	Expect(err).To(BeNil())

	// read assignment
	assID, err := s.mdc.ReadDir(context.TODO(), accountPath(s.cfg.AdminUserID))
	Expect(err).To(BeNil())
	Expect(len(assID)).To(Equal(1))

	// set assignment to user role
	userRoleAssignment := &settingsmsg.UserRoleAssignment{
		AccountUuid: s.cfg.AdminUserID,
		RoleId:      rdefaults.BundleUUIDRoleUser,
	}
	b, err := json.Marshal(userRoleAssignment)
	Expect(err).To(BeNil())
	err = s.mdc.Delete(context.TODO(), assignmentPath(s.cfg.AdminUserID, assID[0]))
	Expect(err).To(BeNil())
	err = s.mdc.SimpleUpload(context.TODO(), assignmentPath(s.cfg.AdminUserID, assID[0]), b)
	Expect(err).To(BeNil())

	// this happens on every Read / Write on the store
	// the actual init is only done if the metadata client has not been initialized before
	// this normally needs a restart of the service
	err = s.initMetadataClient(s.mdc)
	Expect(err).To(BeNil())

	// read assignment id, changes every time the assignment is written
	assID, err = s.mdc.ReadDir(context.TODO(), accountPath(s.cfg.AdminUserID))
	Expect(err).To(BeNil())
	Expect(len(assID)).To(Equal(1))

	// check if the assignment is the admin role again
	ass, err := s.mdc.SimpleDownload(context.TODO(), assignmentPath(s.cfg.AdminUserID, assID[0]))
	Expect(err).To(BeNil())
	Expect(ass).ToNot(BeNil())
	assignment := &settingsmsg.UserRoleAssignment{}
	err = json.Unmarshal(ass, assignment)
	Expect(err).To(BeNil())
	Expect(assignment.RoleId).To(Equal(rdefaults.BundleUUIDRoleAdmin))
}

// newBootstrapStore returns a store with the default roles configured and an
// unattached in-memory client, so tests can seed storage before initialisation.
func newBootstrapStore(t *testing.T) (*Store, *MockedMetadataClient) {
	t.Helper()
	if testtype != "unit" {
		t.Skip("test needs the in-memory MockedMetadataClient")
	}
	s := &Store{
		cfg: defaults.DefaultConfig(),
		l:   &sync.Mutex{},
	}
	s.cfg.Bundles = rdefaults.GenerateBundlesDefaultRoles()
	s.cfg.AdminUserID = "admin"
	return s, newMockedMDC()
}

// The admin already holds Admin plus a second role, e.g. after a partial write.
// userMustHaveAdminRole only checks that Admin is among the assignments, so the
// "exactly one role" invariant stays broken after initialisation.
func TestAdminUserIDInit_AdminWithTwoAssignments(t *testing.T) {
	s, m := newBootstrapStore(t)
	m.seedAssignment(t, s.cfg.AdminUserID, "00000000-0000-0000-0000-0000000000a1", rdefaults.BundleUUIDRoleUser)
	m.seedAssignment(t, s.cfg.AdminUserID, "00000000-0000-0000-0000-0000000000a2", rdefaults.BundleUUIDRoleAdmin)

	require.NoError(t, s.initMetadataClient(m))

	requireSingleAssignment(t, s, s.cfg.AdminUserID, rdefaults.BundleUUIDRoleAdmin)
}

// With SetupDefaultAssignments=true, the demo accounts only get their default
// role if they have none yet. An existing role must not be overwritten.
func TestSetupDefaultAssignments_KeepsExistingRole(t *testing.T) {
	const (
		demoUserWithRole    = "b1f74ec4-dd7e-11ef-a543-03775734d0f7" // default role: User
		demoUserWithoutRole = "056fc874-dd7f-11ef-ba84-af6fca4b7289" // default role: User
	)
	s, m := newBootstrapStore(t)
	s.cfg.SetupDefaultAssignments = true
	existing := "00000000-0000-0000-0000-0000000000b1"
	m.seedAssignment(t, demoUserWithRole, existing, rdefaults.BundleUUIDRoleSpaceAdmin)

	require.NoError(t, s.initMetadataClient(m))

	got := requireSingleAssignment(t, s, demoUserWithRole, rdefaults.BundleUUIDRoleSpaceAdmin)
	require.Equal(t, existing, got.Id)
	requireSingleAssignment(t, s, demoUserWithoutRole, rdefaults.BundleUUIDRoleUser)
	requireSingleAssignment(t, s, s.cfg.AdminUserID, rdefaults.BundleUUIDRoleAdmin)
}

// If the storage cannot be initialised, initMetadataClient must report it and
// leave the store without a client, so the next call retries.
func TestInitMetadataClient_InitError(t *testing.T) {
	s, m := newBootstrapStore(t)
	boom := errors.New("storage-system unavailable")
	m.OnCall("Init", func(string) bool { return true }, func(string) error { return boom })

	err := s.initMetadataClient(m)

	require.ErrorIs(t, err, boom)
	require.Nil(t, s.mdc)
	require.Equal(t, []mdcCall{{Op: "Init", Path: settingsSpaceID}}, m.Calls(),
		"nothing else may be written after Init failed")
}
