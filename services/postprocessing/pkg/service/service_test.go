package service

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/opencloud-eu/opencloud/pkg/log"
	"github.com/opencloud-eu/opencloud/services/postprocessing/pkg/config"
	"github.com/opencloud-eu/opencloud/services/postprocessing/pkg/metrics"
	"github.com/opencloud-eu/opencloud/services/postprocessing/pkg/postprocessing"
	"github.com/opencloud-eu/reva/v3/pkg/events"
	"github.com/opencloud-eu/reva/v3/pkg/events/raw"
	microstore "go-micro.dev/v4/store"
	"go.opentelemetry.io/otel/trace/noop"
)

// Test case IDs (TC-PS-xx) refer to the test design table in
// quality/delivery-1/postprocessing-service.md. Each spec carries its ID both in the
// description and as a Ginkgo label, so a single case can be run with --label-filter.
//
// Every dependency is replaced: the JetStream publisher by testPublisher, the JetStream
// message by spyMsg, the nats kv store by stubStore and the wall clock by fakeClock. Because the clock is fake the fixture uses the
// production defaults (5s base backoff, 1min ack wait) and still no test waits.

var _ = Describe("PostprocessingService", func() {
	var (
		cfg   config.Postprocessing
		pub   *testPublisher
		sto   *stubStore
		clock *fakeClock
		msg   *spyMsg
		pps   *PostprocessingService
	)

	// newService builds a service wired to the doubles only. It deliberately does not go
	// through NewPostprocessingService, which would connect to a running nats.
	newService := func() *PostprocessingService {
		return &PostprocessingService{
			ctx:     context.Background(),
			log:     log.NopLogger(),
			pub:     pub,
			steps:   getSteps(cfg),
			store:   sto,
			c:       cfg,
			tp:      noop.NewTracerProvider(),
			metrics: metrics.New(),
			clock:   clock,
			stopCh:  make(chan struct{}, 1),
		}
	}

	newUploadID := func() string { return "upload-" + uuid.New().String() }

	// bytesReceived is the event that starts a postprocessing chain. Handling it stores the
	// upload and publishes the first StartPostprocessingStep.
	bytesReceived := func(uploadID string) events.Event {
		return event(events.BytesReceived{UploadID: uploadID, Filename: "test.txt", Filesize: 1234})
	}

	stepFinished := func(uploadID string, outcome events.PostprocessingOutcome) events.Event {
		return event(events.PostprocessingStepFinished{
			UploadID:     uploadID,
			FinishedStep: events.PPStepAntivirus,
			Outcome:      outcome,
		})
	}

	uploadReady := func(uploadID string, failed bool) events.Event {
		return event(events.UploadReady{UploadID: uploadID, Filename: "test.txt", Failed: failed})
	}

	// startUpload brings an upload into the store through the service itself, so later
	// events find it in the state the service leaves it in. The publisher and the message
	// used for it are thrown away.
	startUpload := func() string {
		GinkgoHelper()
		id := newUploadID()
		realPub := pps.pub
		pps.pub = &testPublisher{}
		Expect(pps.handleEvent(bytesReceived(id), &spyMsg{})).To(Succeed())
		pps.pub = realPub
		return id
	}

	storedPP := func(id string) *postprocessing.Postprocessing {
		GinkgoHelper()
		pp, err := pps.getPP(sto, id)
		Expect(err).NotTo(HaveOccurred())
		return pp
	}

	BeforeEach(func() {
		cfg = config.Postprocessing{
			Steps:                []string{"virusscan"},
			RetryBackoffDuration: 5 * time.Second,
			MaxRetries:           14,
			PublishMaxRetries:    5,
		}
		cfg.Events.AckWait = time.Minute
		pub = &testPublisher{}
		sto = newStubStore()
		clock = newFakeClock()
		msg = &spyMsg{}
	})

	Describe("acknowledging the source event (decision table)", func() {
		It("TC-PS-01 healthy store and bus: stores the upload, publishes the first step and acks once", Label("TC-PS-01"), func() {
			pps = newService()
			id := newUploadID()

			Expect(pps.handleEvent(bytesReceived(id), msg)).To(Succeed())

			attempts, accepted := pub.stats()
			Expect(attempts).To(Equal(1))
			Expect(accepted).To(HaveLen(1))
			Expect(accepted[0]).To(BeAssignableToTypeOf(events.StartPostprocessingStep{}))
			Expect(accepted[0].(events.StartPostprocessingStep).StepToStart).To(Equal(events.PPStepAntivirus))

			pp := storedPP(id)
			Expect(pp.Status.CurrentStep).To(Equal(events.PPStepAntivirus))
			Expect(pp.StartTime).To(BeTemporally("==", clock.Now()))

			acks, inProgress := msg.counts()
			Expect(acks).To(Equal(1))
			Expect(inProgress).To(Equal(0))
		})

		It("TC-PS-02 store write fails: does not ack, publishes nothing, event error", Label("TC-PS-02"), func() {
			sto.writeErr = errStore
			pps = newService()

			err := pps.handleEvent(bytesReceived(newUploadID()), msg)

			Expect(errors.Is(err, ErrEvent)).To(BeTrue())
			Expect(errors.Is(err, ErrFatal)).To(BeFalse())
			attempts, _ := pub.stats()
			Expect(attempts).To(Equal(0))
			acks, _ := msg.counts()
			Expect(acks).To(Equal(0))
		})

		It("TC-PS-03 transient publish failure: retries, marks the event in progress between attempts, acks once", Label("TC-PS-03"), func() {
			pub.failures = 2
			pps = newService()

			Expect(pps.handleEvent(bytesReceived(newUploadID()), msg)).To(Succeed())

			attempts, accepted := pub.stats()
			Expect(attempts).To(Equal(3))
			Expect(accepted).To(HaveLen(1))
			acks, inProgress := msg.counts()
			Expect(acks).To(Equal(1))
			Expect(inProgress).To(Equal(2))
		})

		It("TC-PS-04 publish retries exhausted: does not ack, fatal error", Label("TC-PS-04"), func() {
			pub.failures = 1000
			pps = newService()

			err := pps.handleEvent(bytesReceived(newUploadID()), msg)

			Expect(errors.Is(err, ErrFatal)).To(BeTrue())
			// the initial attempt plus PublishMaxRetries retries
			attempts, accepted := pub.stats()
			Expect(attempts).To(Equal(6))
			Expect(accepted).To(BeEmpty())
			acks, inProgress := msg.counts()
			Expect(acks).To(Equal(0))
			Expect(inProgress).To(Equal(5))
		})

		It("TC-PS-05 publish fails while the service is stopping: does not ack, event error instead of fatal", Label("TC-PS-05"), func() {
			pub.failures = 1000
			clock.hold = true // the stop must end the wait, not the clock
			pps = newService()
			pps.Close()

			err := pps.handleEvent(bytesReceived(newUploadID()), msg)

			Expect(errors.Is(err, ErrEvent)).To(BeTrue())
			Expect(errors.Is(err, ErrFatal)).To(BeFalse())
			attempts, _ := pub.stats()
			Expect(attempts).To(Equal(1))
			acks, _ := msg.counts()
			Expect(acks).To(Equal(0))
		})

		It("TC-PS-06 publish fails after the context is cancelled: does not ack, event error instead of fatal", Label("TC-PS-06"), func() {
			pub.failures = 1000
			clock.hold = true
			pps = newService()
			ctx, cancel := context.WithCancel(context.Background())
			pps.ctx = ctx
			cancel()

			err := pps.handleEvent(bytesReceived(newUploadID()), msg)

			Expect(errors.Is(err, ErrEvent)).To(BeTrue())
			Expect(errors.Is(err, ErrFatal)).To(BeFalse())
			attempts, _ := pub.stats()
			Expect(attempts).To(Equal(1))
			acks, _ := msg.counts()
			Expect(acks).To(Equal(0))
		})

		It("TC-PS-07 retrying disabled (PublishMaxRetries 0): one attempt, no wait, fatal", Label("TC-PS-07"), func() {
			cfg.PublishMaxRetries = 0
			pub.failures = 1000
			pps = newService()

			err := pps.handleEvent(bytesReceived(newUploadID()), msg)

			Expect(errors.Is(err, ErrFatal)).To(BeTrue())
			attempts, _ := pub.stats()
			Expect(attempts).To(Equal(1))
			Expect(clock.Waits()).To(BeEmpty())
			acks, inProgress := msg.counts()
			Expect(acks).To(Equal(0))
			Expect(inProgress).To(Equal(0))
		})

		It("TC-PS-08 a failing in-progress mark does not stop the retries", Label("TC-PS-08"), func() {
			pub.failures = 1
			msg.inProgressErr = errors.New("nats: connection closed")
			pps = newService()

			Expect(pps.handleEvent(bytesReceived(newUploadID()), msg)).To(Succeed())

			attempts, _ := pub.stats()
			Expect(attempts).To(Equal(2))
			acks, _ := msg.counts()
			Expect(acks).To(Equal(1))
		})

		It("TC-PS-09 a failing ack is only logged", Label("TC-PS-09"), func() {
			msg.ackErr = errors.New("nats: message was already acknowledged")
			pps = newService()

			Expect(pps.handleEvent(bytesReceived(newUploadID()), msg)).To(Succeed())
			acks, _ := msg.counts()
			Expect(acks).To(Equal(1))
		})

		It("TC-PS-10 unknown upload: event error but the event is still acked", Label("TC-PS-10", "characterization"), func() {
			// Contrast with TC-PS-02: a failed store *write* is redelivered, a failed store
			// *read* is acknowledged and therefore dropped (F5).
			pps = newService()

			err := pps.handleEvent(stepFinished(newUploadID(), events.PPOutcomeContinue), msg)

			Expect(errors.Is(err, ErrEvent)).To(BeTrue())
			acks, _ := msg.counts()
			Expect(acks).To(Equal(1))
		})

		It("TC-PS-11 transient store read error: event error and the event is acked (dropped)", Label("TC-PS-11", "characterization"), func() {
			pps = newService()
			id := startUpload()
			sto.readErr = errStore

			err := pps.handleEvent(stepFinished(id, events.PPOutcomeContinue), msg)

			Expect(errors.Is(err, ErrEvent)).To(BeTrue())
			acks, _ := msg.counts()
			Expect(acks).To(Equal(1))
			_, accepted := pub.stats()
			Expect(accepted).To(BeEmpty())
		})

		It("TC-PS-12 the raw.Event adapter hands the payload over even without a jetstream message", Label("TC-PS-12"), func() {
			// processEvent is what the worker loop calls. A raw.Event built outside reva has no
			// message, so its Ack fails; that failure is only logged.
			pps = newService()

			Expect(pps.processEvent(raw.Event{Event: bytesReceived(newUploadID())})).To(Succeed())
			_, accepted := pub.stats()
			Expect(accepted).To(HaveLen(1))
		})
	})

	Describe("waiting between publish attempts", func() {
		DescribeTable("the waits requested from the clock",
			func(base, ackWait time.Duration, maxRetries, failures int, expected []time.Duration) {
				cfg.RetryBackoffDuration = base
				cfg.Events.AckWait = ackWait
				cfg.PublishMaxRetries = maxRetries
				pub.failures = failures
				pps = newService()

				_ = pps.handleEvent(bytesReceived(newUploadID()), msg)

				Expect(clock.Waits()).To(Equal(expected))
			},
			Entry("TC-PS-13 grows exponentially below the cap", Label("TC-PS-13"),
				5*time.Second, time.Minute, 5, 3,
				[]time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second}),
			Entry("TC-PS-14 is capped at half the ack wait", Label("TC-PS-14"),
				5*time.Second, time.Minute, 5, 5,
				[]time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 30 * time.Second, 30 * time.Second}),
			Entry("TC-PS-15 a wait equal to the cap is kept", Label("TC-PS-15"),
				10*time.Second, 20*time.Second, 1, 1,
				[]time.Duration{10 * time.Second}),
			Entry("TC-PS-16 no cap when the ack wait is 0", Label("TC-PS-16"),
				5*time.Second, time.Duration(0), 5, 5,
				[]time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second}),
			Entry("TC-PS-17 no cap when half the ack wait rounds to 0", Label("TC-PS-17", "characterization"),
				5*time.Second, time.Nanosecond, 2, 2,
				[]time.Duration{5 * time.Second, 10 * time.Second}),
			Entry("TC-PS-18 the smallest non-zero cap applies", Label("TC-PS-18"),
				5*time.Second, 2*time.Nanosecond, 2, 2,
				[]time.Duration{time.Nanosecond, time.Nanosecond}),
			Entry("TC-PS-19 no wait after the last attempt", Label("TC-PS-19"),
				5*time.Second, time.Minute, 2, 1000,
				[]time.Duration{5 * time.Second, 10 * time.Second}),
		)

		It("TC-PS-20 an overflowing backoff escapes the cap and is not waited at all", Label("TC-PS-20", "characterization"), func() {
			// Contract (publishWithRetry doc comment): never wait longer than half the ack
			// wait, exponential backoff otherwise. With the default 5s base the 32nd backoff
			// overflows int64 nanoseconds and turns negative; `backoff > maxBackoff` is false
			// for a negative value, so the cap does not catch it (F4).
			cfg.PublishMaxRetries = 32
			pub.failures = 1000
			pps = newService()

			_ = pps.handleEvent(bytesReceived(newUploadID()), msg)

			waits := clock.Waits()
			Expect(waits).To(HaveLen(32))
			Expect(waits[30]).To(Equal(30 * time.Second))
			Expect(waits[31]).To(BeNumerically("<", 0))
		})

		It("TC-PS-21 stopping the service ends a wait in progress", Label("TC-PS-21"), func() {
			pub.failures = 1000
			clock.hold = true
			pps = newService()

			done := make(chan error, 1)
			go func() { done <- pps.handleEvent(bytesReceived(newUploadID()), msg) }()

			Eventually(clock.Waits).Should(HaveLen(1))
			Consistently(done).ShouldNot(Receive())

			pps.Close()

			var err error
			Eventually(done).Should(Receive(&err))
			Expect(errors.Is(err, ErrEvent)).To(BeTrue())
			attempts, _ := pub.stats()
			Expect(attempts).To(Equal(1))
		})
	})

	Describe("handling PostprocessingStepFinished", func() {
		It("TC-PS-22 an on-demand scan without an upload is acked and ignored", Label("TC-PS-22"), func() {
			pps = newService()

			Expect(pps.handleEvent(stepFinished("", events.PPOutcomeContinue), msg)).To(Succeed())
			Expect(pub.acceptedEvents()).To(BeEmpty())
			acks, _ := msg.counts()
			Expect(acks).To(Equal(1))
		})

		It("TC-PS-23 continue on the last step publishes finished and stores the state", Label("TC-PS-23"), func() {
			pps = newService()
			id := startUpload()

			Expect(pps.handleEvent(stepFinished(id, events.PPOutcomeContinue), msg)).To(Succeed())

			accepted := pub.acceptedEvents()
			Expect(accepted).To(HaveLen(1))
			Expect(accepted[0]).To(BeAssignableToTypeOf(events.PostprocessingFinished{}))
			Expect(storedPP(id).Status.CurrentStep).To(Equal(events.PPStepFinished))
		})

		It("TC-PS-24 retry publishes the retry at once and restarts the step only after the backoff", Label("TC-PS-24"), func() {
			clock.hold = true
			pps = newService()
			id := startUpload()

			Expect(pps.handleEvent(stepFinished(id, events.PPOutcomeRetry), msg)).To(Succeed())

			accepted := pub.acceptedEvents()
			Expect(accepted).To(HaveLen(1))
			Expect(accepted[0]).To(BeAssignableToTypeOf(events.PostprocessingRetry{}))
			Expect(accepted[0].(events.PostprocessingRetry).BackoffDuration).To(Equal(5 * time.Second))

			// the restart waits on the clock for the backoff of the first failure
			Eventually(clock.Waits).Should(Equal([]time.Duration{5 * time.Second}))
			Consistently(pub.acceptedEvents).Should(HaveLen(1))

			clock.Release()

			Eventually(pub.acceptedEvents).Should(HaveLen(2))
			restart := pub.acceptedEvents()[1]
			Expect(restart).To(BeAssignableToTypeOf(events.StartPostprocessingStep{}))
			Expect(restart.(events.StartPostprocessingStep).UploadID).To(Equal(id))
			Expect(restart.(events.StartPostprocessingStep).StepToStart).To(Equal(events.PPStepAntivirus))
			Expect(storedPP(id).Failures).To(Equal(1))
		})
	})

	Describe("handling StartPostprocessingStep", func() {
		It("TC-PS-25 a step other than delay is acked and ignored", Label("TC-PS-25"), func() {
			pps = newService()

			Expect(pps.handleEvent(event(events.StartPostprocessingStep{
				UploadID:    newUploadID(),
				StepToStart: events.PPStepAntivirus,
			}), msg)).To(Succeed())
			Expect(pub.acceptedEvents()).To(BeEmpty())
			acks, _ := msg.counts()
			Expect(acks).To(Equal(1))
		})

		It("TC-PS-46 the delay step publishes the step that follows it", Label("TC-PS-46", "race"), func() {
			// The wait itself belongs to Postprocessing.Delay (TC-PP-31..34). An upload read
			// from the store carries no clock, so it uses the real one; delay 0 keeps it instant.
			cfg.Steps = []string{"delay", "virusscan"}
			cfg.Delayprocessing = 0
			pps = newService()
			id := startUpload()

			Expect(pps.handleEvent(event(events.StartPostprocessingStep{
				UploadID:    id,
				StepToStart: events.PPStepDelay,
			}), msg)).To(Succeed())

			Eventually(pub.acceptedEvents).Should(HaveLen(1))
			next := pub.acceptedEvents()[0]
			Expect(next).To(BeAssignableToTypeOf(events.StartPostprocessingStep{}))
			Expect(next.(events.StartPostprocessingStep).StepToStart).To(Equal(events.PPStepAntivirus))
		})

		It("TC-PS-47 the delay step of an unknown upload is an event error", Label("TC-PS-47"), func() {
			pps = newService()

			err := pps.handleEvent(event(events.StartPostprocessingStep{
				UploadID:    newUploadID(),
				StepToStart: events.PPStepDelay,
			}), msg)
			Expect(errors.Is(err, ErrEvent)).To(BeTrue())
		})
	})

	Describe("default clock", func() {
		It("TC-PS-48 falls back to the real clock when none was set", Label("TC-PS-48"), func() {
			pps = newService()
			pps.clock = nil
			before := time.Now()
			id := newUploadID()

			Expect(pps.handleEvent(bytesReceived(id), msg)).To(Succeed())
			Expect(storedPP(id).StartTime).To(BeTemporally(">=", before))
		})
	})

	Describe("handling UploadReady", func() {
		It("TC-PS-26 ignores an event that does not belong to an upload", Label("TC-PS-26"), func() {
			pps = newService()

			Expect(pps.handleEvent(uploadReady("", false), msg)).To(Succeed())
		})

		It("TC-PS-27 fails for an upload it does not know", Label("TC-PS-27"), func() {
			pps = newService()

			err := pps.handleEvent(uploadReady(newUploadID(), false), msg)
			Expect(errors.Is(err, ErrEvent)).To(BeTrue())
		})

		It("TC-PS-28 a failed upload is kept, marked finished and counted with its duration", Label("TC-PS-28"), func() {
			pps = newService()
			id := startUpload()
			count, sum := histogramValue(pps.metrics.Duration, "failed")
			finished := counterValue(pps.metrics.Finished, "failed")
			clock.Advance(42 * time.Second)

			Expect(pps.handleEvent(uploadReady(id, true), msg)).To(Succeed())

			Expect(storedPP(id).Finished).To(BeTrue())
			Expect(counterValue(pps.metrics.Finished, "failed")).To(Equal(finished + 1))
			newCount, newSum := histogramValue(pps.metrics.Duration, "failed")
			Expect(newCount).To(Equal(count + 1))
			Expect(newSum - sum).To(BeNumerically("~", 42, 1e-9))
		})

		It("TC-PS-29 a successful upload is deleted from the store and counted with its duration", Label("TC-PS-29"), func() {
			pps = newService()
			id := startUpload()
			count, sum := histogramValue(pps.metrics.Duration, "succeeded")
			finished := counterValue(pps.metrics.Finished, "succeeded")
			clock.Advance(7 * time.Second)

			Expect(pps.handleEvent(uploadReady(id, false), msg)).To(Succeed())

			Expect(sto.deletedKeys()).To(Equal([]string{id}))
			Expect(counterValue(pps.metrics.Finished, "succeeded")).To(Equal(finished + 1))
			newCount, newSum := histogramValue(pps.metrics.Duration, "succeeded")
			Expect(newCount).To(Equal(count + 1))
			Expect(newSum - sum).To(BeNumerically("~", 7, 1e-9))
		})

		It("TC-PS-30 a successful upload is written back to the store right after its deletion", Label("TC-PS-30", "characterization"), func() {
			// Contract (code comment): "the storage provider thinks the upload is done - so no
			// need to keep it any more". Observed: after the delete, the switch falls through
			// to storePP, which stores the upload again, unfinished (F6). A later resume of
			// that upload then re-publishes its last event.
			pps = newService()
			id := startUpload()
			Expect(pps.handleEvent(stepFinished(id, events.PPOutcomeContinue), &spyMsg{})).To(Succeed())
			pub.accepted = nil

			Expect(pps.handleEvent(uploadReady(id, false), msg)).To(Succeed())

			Expect(sto.deletedKeys()).To(Equal([]string{id}))
			pp := storedPP(id)
			Expect(pp.Finished).To(BeFalse())

			Expect(pps.handleEvent(event(events.ResumePostprocessing{UploadID: id}), &spyMsg{})).To(Succeed())
			accepted := pub.acceptedEvents()
			Expect(accepted).To(HaveLen(1))
			Expect(accepted[0]).To(BeAssignableToTypeOf(events.PostprocessingFinished{}))
		})

		It("TC-PS-31 a failing delete is an event error and the event is still acked", Label("TC-PS-31", "characterization"), func() {
			pps = newService()
			id := startUpload()
			sto.deleteErr = errStore

			err := pps.handleEvent(uploadReady(id, false), msg)

			Expect(errors.Is(err, ErrEvent)).To(BeTrue())
			acks, _ := msg.counts()
			Expect(acks).To(Equal(1))
		})
	})

	Describe("handling ResumePostprocessing", func() {
		It("TC-PS-32 resumes an upload in progress by re-publishing its current step", Label("TC-PS-32"), func() {
			pps = newService()
			id := startUpload()

			Expect(pps.handleEvent(event(events.ResumePostprocessing{UploadID: id}), msg)).To(Succeed())

			accepted := pub.acceptedEvents()
			Expect(accepted).To(HaveLen(1))
			Expect(accepted[0].(events.StartPostprocessingStep).StepToStart).To(Equal(events.PPStepAntivirus))
		})

		It("TC-PS-33 does not resume a finished upload", Label("TC-PS-33"), func() {
			pps = newService()
			id := startUpload()
			Expect(pps.handleEvent(uploadReady(id, true), &spyMsg{})).To(Succeed())

			Expect(pps.handleEvent(event(events.ResumePostprocessing{UploadID: id}), msg)).To(Succeed())
			Expect(pub.acceptedEvents()).To(BeEmpty())
		})

		It("TC-PS-34 asks for a restart of an upload it does not know", Label("TC-PS-34"), func() {
			pps = newService()
			id := newUploadID()

			Expect(pps.handleEvent(event(events.ResumePostprocessing{UploadID: id}), msg)).To(Succeed())

			accepted := pub.acceptedEvents()
			Expect(accepted).To(HaveLen(1))
			Expect(accepted[0]).To(BeAssignableToTypeOf(events.RestartPostprocessing{}))
			Expect(accepted[0].(events.RestartPostprocessing).UploadID).To(Equal(id))
		})

		It("TC-PS-35 resumes by step only the uploads that are at that step", Label("TC-PS-35"), func() {
			pps = newService()
			atScan1, atScan2, done := startUpload(), startUpload(), startUpload()
			Expect(pps.handleEvent(stepFinished(done, events.PPOutcomeContinue), &spyMsg{})).To(Succeed())
			pub.accepted = nil

			Expect(pps.handleEvent(event(events.ResumePostprocessing{Step: events.PPStepAntivirus}), msg)).To(Succeed())

			var ids []string
			for _, ev := range pub.acceptedEvents() {
				Expect(ev).To(BeAssignableToTypeOf(events.StartPostprocessingStep{}))
				ids = append(ids, ev.(events.StartPostprocessingStep).UploadID)
			}
			Expect(ids).To(ConsistOf(atScan1, atScan2))
		})

		It("TC-PS-36 resuming by step resumes nothing when the store cannot be listed", Label("TC-PS-36"), func() {
			pps = newService()
			startUpload()
			sto.listErr = errStore

			Expect(pps.handleEvent(event(events.ResumePostprocessing{Step: events.PPStepAntivirus}), msg)).To(Succeed())
			Expect(pub.acceptedEvents()).To(BeEmpty())
		})
	})

	Describe("reading an upload from the store (getPP)", func() {
		valid, _ := json.Marshal(postprocessing.Postprocessing{ID: "upload-1"})

		DescribeTable("store read outcomes",
			func(readErr error, records []*microstore.Record, check func(*postprocessing.Postprocessing, error)) {
				sto.readErr = readErr
				sto.readRecords = records
				pps = newService()

				check(pps.getPP(sto, "upload-1"))
			},
			Entry("TC-PS-37 key not found is ErrNotFound", Label("TC-PS-37"),
				microstore.ErrNotFound, nil,
				func(pp *postprocessing.Postprocessing, err error) { Expect(err).To(MatchError(ErrNotFound)) }),
			Entry("TC-PS-38 any other read error is returned as is", Label("TC-PS-38"),
				errStore, nil,
				func(pp *postprocessing.Postprocessing, err error) { Expect(err).To(MatchError(errStore)) }),
			Entry("TC-PS-39 zero records is ErrNotFound", Label("TC-PS-39"),
				nil, []*microstore.Record{},
				func(pp *postprocessing.Postprocessing, err error) { Expect(err).To(MatchError(ErrNotFound)) }),
			Entry("TC-PS-40 one valid record is decoded", Label("TC-PS-40"),
				nil, []*microstore.Record{{Key: "upload-1", Value: valid}},
				func(pp *postprocessing.Postprocessing, err error) {
					Expect(err).NotTo(HaveOccurred())
					Expect(pp.ID).To(Equal("upload-1"))
				}),
			Entry("TC-PS-41 two records is an error", Label("TC-PS-41"),
				nil, []*microstore.Record{{Key: "upload-1", Value: valid}, {Key: "upload-1", Value: valid}},
				func(pp *postprocessing.Postprocessing, err error) {
					Expect(err).To(MatchError(ContainSubstring("expected only one result")))
				}),
			Entry("TC-PS-42 a record that is not json is an error", Label("TC-PS-42"),
				nil, []*microstore.Record{{Key: "upload-1", Value: []byte("{not json")}},
				func(pp *postprocessing.Postprocessing, err error) {
					var syntaxErr *json.SyntaxError
					Expect(errors.As(err, &syntaxErr)).To(BeTrue())
				}),
		)
	})

	Describe("the worker loop (Run / Close)", func() {
		It("TC-PS-43 processes the queued events and returns once the event channel is closed", Label("TC-PS-43"), func() {
			cfg.Workers = 1
			pps = newService()
			ch := make(chan raw.Event, 2)
			ch <- raw.Event{Event: bytesReceived(newUploadID())}
			ch <- raw.Event{Event: bytesReceived(newUploadID())}
			close(ch)
			pps.events = ch

			Expect(pps.Run()).To(Succeed())
			Expect(pub.acceptedEvents()).To(HaveLen(2))
		})

		It("TC-PS-44 returns after Close even though the event channel stays open", Label("TC-PS-44"), func() {
			cfg.Workers = 2
			pps = newService()
			pps.events = make(chan raw.Event)

			done := make(chan error, 1)
			go func() { done <- pps.Run() }()
			Consistently(done).ShouldNot(Receive())

			pps.Close()
			Eventually(done).Should(Receive(BeNil()))
		})

		It("TC-PS-45 Close can be called more than once", Label("TC-PS-45"), func() {
			pps = newService()

			Expect(func() { pps.Close(); pps.Close() }).NotTo(Panic())
		})
	})
})
