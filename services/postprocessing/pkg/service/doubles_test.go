package service

import (
	"errors"
	"reflect"
	"sync"
	"time"

	"github.com/google/uuid"
	. "github.com/onsi/gomega"
	"github.com/opencloud-eu/reva/v3/pkg/events"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	microevents "go-micro.dev/v4/events"
	microstore "go-micro.dev/v4/store"
)

// Test doubles for the dependencies of the PostprocessingService. Each one replaces a part of
// NATS JetStream, the store or the clock, so the service can be tested without any of them.

// errPublish is the error a nats publish returns when the ack of the message was not
// received in time. It is transient: the very next publish usually succeeds.
var errPublish = errors.New("nats: timeout")

// errStore is a generic failure of the store backend (e.g. the nats kv bucket is unavailable).
var errStore = errors.New("store: unavailable")

// testPublisher replaces the JetStream publisher (events.Publisher). It is a stub that fails
// the first `failures` publish attempts and a spy that records everything it accepted.
type testPublisher struct {
	mu       sync.Mutex
	failures int
	attempts int
	accepted []any
}

func (p *testPublisher) Publish(_ string, ev any, _ ...microevents.PublishOption) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.attempts++
	if p.attempts <= p.failures {
		return errPublish
	}
	p.accepted = append(p.accepted, ev)
	return nil
}

func (p *testPublisher) stats() (attempts int, accepted []any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.attempts, append([]any(nil), p.accepted...)
}

func (p *testPublisher) acceptedEvents() []any {
	_, accepted := p.stats()
	return accepted
}

// spyMsg replaces the JetStream message an event arrived with (the acker). It records how
// often the service acknowledged it or marked it as in progress, and can be told to fail.
type spyMsg struct {
	mu            sync.Mutex
	acks          int
	inProgress    int
	ackErr        error
	inProgressErr error
}

func (m *spyMsg) Ack() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.acks++
	return m.ackErr
}

func (m *spyMsg) InProgress() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inProgress++
	return m.inProgressErr
}

func (m *spyMsg) counts() (acks, inProgress int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.acks, m.inProgress
}

// fakeClock replaces the wall clock. Now returns a fixed instant that only moves with
// Advance. After records every requested wait; by default the wait is over at once, with
// hold set the returned channels only fire on Release, so the test decides when time passes.
type fakeClock struct {
	mu    sync.Mutex
	now   time.Time
	waits []time.Duration
	hold  bool
	held  []chan time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	ch := make(chan time.Time, 1)
	c.waits = append(c.waits, d)
	if c.hold {
		c.held = append(c.held, ch)
	} else {
		ch <- c.now.Add(d)
	}
	return ch
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// Release ends every wait that is currently held.
func (c *fakeClock) Release() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ch := range c.held {
		ch <- c.now
	}
	c.held = nil
}

func (c *fakeClock) Waits() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.waits...)
}

// stubStore replaces the nats kv store. It is a fake (a map keyed by upload ID) that can be
// told to fail a single operation or to return records the real store would never return,
// and a spy on the deleted keys.
//
// It does not wrap go-micro's memory store on purpose: that store builds its keys with
// filepath.Join but filters List on "/", so on Windows List never returns anything.
type stubStore struct {
	mu      sync.Mutex
	records map[string][]byte
	deleted []string

	readErr     error
	readRecords []*microstore.Record // returned by Read instead of the stored ones when set
	writeErr    error
	deleteErr   error
	listErr     error
}

func newStubStore() *stubStore {
	return &stubStore{records: map[string][]byte{}}
}

func (s *stubStore) Read(key string, _ ...microstore.ReadOption) ([]*microstore.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readErr != nil {
		return nil, s.readErr
	}
	if s.readRecords != nil {
		return s.readRecords, nil
	}
	v, ok := s.records[key]
	if !ok {
		return nil, microstore.ErrNotFound
	}
	return []*microstore.Record{{Key: key, Value: append([]byte(nil), v...)}}, nil
}

func (s *stubStore) Write(r *microstore.Record, _ ...microstore.WriteOption) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeErr != nil {
		return s.writeErr
	}
	s.records[r.Key] = append([]byte(nil), r.Value...)
	return nil
}

func (s *stubStore) Delete(key string, _ ...microstore.DeleteOption) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deleteErr != nil {
		return s.deleteErr
	}
	s.deleted = append(s.deleted, key)
	delete(s.records, key)
	return nil
}

func (s *stubStore) List(_ ...microstore.ListOption) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listErr != nil {
		return nil, s.listErr
	}
	keys := make([]string, 0, len(s.records))
	for k := range s.records {
		keys = append(keys, k)
	}
	return keys, nil
}

func (s *stubStore) deletedKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.deleted...)
}

// The service never calls the rest of the store interface.
func (s *stubStore) Init(...microstore.Option) error { return nil }
func (s *stubStore) Options() microstore.Options     { return microstore.Options{} }
func (s *stubStore) Close() error                    { return nil }
func (s *stubStore) String() string                  { return "stub" }

// event wraps a payload the way the reva event consumer delivers it.
func event(ev any) events.Event {
	return events.Event{
		ID:    uuid.New().String(),
		Type:  reflect.TypeOf(ev).String(),
		Event: ev,
	}
}

// The metrics are process wide prometheus collectors, so tests compare before/after values.

func counterValue(c *prometheus.CounterVec, label string) float64 {
	var m dto.Metric
	Expect(c.WithLabelValues(label).Write(&m)).To(Succeed())
	return m.GetCounter().GetValue()
}

func histogramValue(h *prometheus.HistogramVec, label string) (count uint64, sum float64) {
	var m dto.Metric
	Expect(h.WithLabelValues(label).(prometheus.Metric).Write(&m)).To(Succeed())
	return m.GetHistogram().GetSampleCount(), m.GetHistogram().GetSampleSum()
}
