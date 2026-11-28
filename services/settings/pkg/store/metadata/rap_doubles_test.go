package store

// Test doubles for the role-assignment and permission store suite (rap_store_test.go).
//
// The store has one real dependency, MetadataClient (in production the CS3
// metadata storage behind CachedMDC). Each double below replaces it:
//
//	fakeMDC  FAKE   working in-memory folder tree with the same NotFound semantics
//	spyMDC   SPY    forwards to another client and records every call
//	stubMDC  STUB   forwards to another client but returns a canned error for one op/path
//
// The logger is a DUMMY (olog.NopLogger): it is passed because the Store needs one,
// and no test depends on anything written to it.

import (
	"context"
	"math/rand"
	"path"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	olog "github.com/opencloud-eu/opencloud/pkg/log"
	"github.com/opencloud-eu/opencloud/services/settings/pkg/config"
	"github.com/opencloud-eu/reva/v3/pkg/errtypes"
)

// ---------------------------------------------------------------- FAKE

// fakeMDC is an in-memory MetadataClient. Assumptions it makes about CS3:
//   - a missing path gives errtypes.NotFound on ReadDir, SimpleDownload and Delete
//   - paths are cleaned, so "settings/accounts/" is the folder "settings/accounts"
//   - Delete removes a folder and everything below it
//   - ReadDir returns the names of direct children, each once
//   - MakeDirIfNotExist is idempotent
type fakeMDC struct {
	mu    sync.Mutex
	files map[string][]byte
	dirs  map[string]bool
}

func newFakeMDC() *fakeMDC {
	return &fakeMDC{files: map[string][]byte{}, dirs: map[string]bool{}}
}

func clean(p string) string { return strings.Trim(path.Clean("/"+p), "/") }

func (f *fakeMDC) mkdirAll(p string) {
	for p != "" && p != "." {
		f.dirs[p] = true
		p = path.Dir(p)
		if p == "." {
			return
		}
	}
}

func (f *fakeMDC) Init(context.Context, string) error { return nil }

func (f *fakeMDC) SimpleDownload(_ context.Context, id string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if b, ok := f.files[clean(id)]; ok {
		return b, nil
	}
	return nil, errtypes.NotFound(id)
}

func (f *fakeMDC) SimpleUpload(_ context.Context, id string, content []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	id = clean(id)
	f.mkdirAll(path.Dir(id))
	f.files[id] = content
	return nil
}

func (f *fakeMDC) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	id = clean(id)
	if _, ok := f.files[id]; !ok && !f.dirs[id] {
		return errtypes.NotFound(id)
	}
	delete(f.files, id)
	delete(f.dirs, id)
	for k := range f.files {
		if strings.HasPrefix(k, id+"/") {
			delete(f.files, k)
		}
	}
	for k := range f.dirs {
		if strings.HasPrefix(k, id+"/") {
			delete(f.dirs, k)
		}
	}
	return nil
}

func (f *fakeMDC) ReadDir(_ context.Context, id string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id = clean(id)
	if !f.dirs[id] {
		return nil, errtypes.NotFound(id)
	}
	seen := map[string]bool{}
	for _, m := range []map[string]bool{f.dirs, keys(f.files)} {
		for k := range m {
			if strings.HasPrefix(k, id+"/") {
				seen[strings.SplitN(strings.TrimPrefix(k, id+"/"), "/", 2)[0]] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out) // deterministic order; CS3 promises none, so tests never rely on it
	return out, nil
}

func (f *fakeMDC) MakeDirIfNotExist(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mkdirAll(clean(id))
	return nil
}

func keys(m map[string][]byte) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

// ---------------------------------------------------------------- SPY

type call struct{ op, path string }

// spyMDC records every call and forwards it unchanged.
type spyMDC struct {
	next  MetadataClient
	calls []call
}

func (s *spyMDC) rec(op, p string) { s.calls = append(s.calls, call{op, p}) }

func (s *spyMDC) Init(ctx context.Context, id string) error {
	s.rec("Init", id)
	return s.next.Init(ctx, id)
}
func (s *spyMDC) SimpleDownload(ctx context.Context, id string) ([]byte, error) {
	s.rec("SimpleDownload", id)
	return s.next.SimpleDownload(ctx, id)
}
func (s *spyMDC) SimpleUpload(ctx context.Context, id string, c []byte) error {
	s.rec("SimpleUpload", id)
	return s.next.SimpleUpload(ctx, id, c)
}
func (s *spyMDC) Delete(ctx context.Context, id string) error {
	s.rec("Delete", id)
	return s.next.Delete(ctx, id)
}
func (s *spyMDC) ReadDir(ctx context.Context, id string) ([]string, error) {
	s.rec("ReadDir", id)
	return s.next.ReadDir(ctx, id)
}
func (s *spyMDC) MakeDirIfNotExist(ctx context.Context, id string) error {
	s.rec("MakeDirIfNotExist", id)
	return s.next.MakeDirIfNotExist(ctx, id)
}

// ---------------------------------------------------------------- STUB

// stubMDC returns a canned error for one operation on one path and forwards
// everything else.
type stubMDC struct {
	next MetadataClient
	op   string
	path string
	err  error
}

func (s *stubMDC) canned(op, p string) error {
	if op == s.op && clean(p) == clean(s.path) {
		return s.err
	}
	return nil
}

func (s *stubMDC) Init(ctx context.Context, id string) error {
	if err := s.canned("Init", id); err != nil {
		return err
	}
	return s.next.Init(ctx, id)
}
func (s *stubMDC) SimpleDownload(ctx context.Context, id string) ([]byte, error) {
	if err := s.canned("SimpleDownload", id); err != nil {
		return nil, err
	}
	return s.next.SimpleDownload(ctx, id)
}
func (s *stubMDC) SimpleUpload(ctx context.Context, id string, c []byte) error {
	if s.op == "SimpleUpload" { // any upload fails: the assignment id is random
		return s.err
	}
	return s.next.SimpleUpload(ctx, id, c)
}
func (s *stubMDC) Delete(ctx context.Context, id string) error {
	if err := s.canned("Delete", id); err != nil {
		return err
	}
	return s.next.Delete(ctx, id)
}
func (s *stubMDC) ReadDir(ctx context.Context, id string) ([]string, error) {
	if err := s.canned("ReadDir", id); err != nil {
		return nil, err
	}
	return s.next.ReadDir(ctx, id)
}
func (s *stubMDC) MakeDirIfNotExist(ctx context.Context, id string) error {
	if err := s.canned("MakeDirIfNotExist", id); err != nil {
		return err
	}
	return s.next.MakeDirIfNotExist(ctx, id)
}

// ---------------------------------------------------------------- helpers

// newStoreWith builds a Store around the given client, skipping the lazy CS3
// initialisation (and with it the CachedMDC and its TTL clock).
func newStoreWith(t *testing.T, mdc MetadataClient, serviceAccounts ...string) *Store {
	t.Helper()
	return &Store{
		Logger: olog.NopLogger(), // DUMMY
		mdc:    mdc,
		cfg:    &config.Config{ServiceAccountIDs: serviceAccounts},
		l:      &sync.Mutex{},
	}
}

// seededUUIDs makes uuid.NewString reproducible for one test and restores the
// default random source afterwards.
func seededUUIDs(t *testing.T) {
	t.Helper()
	uuid.SetRand(rand.New(rand.NewSource(42)))
	t.Cleanup(func() { uuid.SetRand(nil) })
}
