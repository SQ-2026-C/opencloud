package postprocessing

import (
	"time"

	user "github.com/cs3org/go-cs3apis/cs3/identity/user/v1beta1"
	provider "github.com/cs3org/go-cs3apis/cs3/storage/provider/v1beta1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/opencloud-eu/opencloud/services/postprocessing/pkg/config"
	"github.com/opencloud-eu/reva/v3/pkg/events"
)

// Test case IDs (TC-PP-xx) refer to the test design table in
// quality/delivery-1/postprocessing-state-machine.md. Each spec carries its ID both in the
// description and as a Ginkgo label, so a single case can be run with --label-filter.

// fakeClock replaces the real clock. Sleep reports the requested duration and then blocks
// until the test releases it, so the test decides when the waiting is over.
type fakeClock struct {
	requested chan time.Duration
	release   chan struct{}
}

func newFakeClock() *fakeClock {
	return &fakeClock{
		requested: make(chan time.Duration, 1),
		release:   make(chan struct{}),
	}
}

func (c *fakeClock) Sleep(d time.Duration) {
	c.requested <- d
	<-c.release
}

var (
	policies  = events.PPStepPolicies
	virusscan = events.PPStepAntivirus
	delay     = events.PPStepDelay
	finished  = events.PPStepFinished
)

var (
	executingUser     = &user.User{Id: &user.UserId{OpaqueId: "user-1"}}
	impersonatingUser = &user.User{Id: &user.UserId{OpaqueId: "user-2"}}
	resourceID        = &provider.ResourceId{StorageId: "s", SpaceId: "sp", OpaqueId: "o"}
)

// newPP builds a postprocessing with fixed upload data, so the payloads of the emitted
// events can be checked field by field.
func newPP(cfg config.Postprocessing, steps ...events.Postprocessingstep) *Postprocessing {
	pp := New(cfg)
	pp.ID = "upload-1"
	pp.URL = "https://example.test/upload-1"
	pp.User = executingUser
	pp.ImpersonatingUser = impersonatingUser
	pp.Filename = "report.pdf"
	pp.Filesize = 4096
	pp.ResourceID = resourceID
	pp.Steps = steps
	return pp
}

func finishedStep(step events.Postprocessingstep, outcome events.PostprocessingOutcome) events.PostprocessingStepFinished {
	return events.PostprocessingStepFinished{
		UploadID:     "upload-1",
		FinishedStep: step,
		Outcome:      outcome,
	}
}

func expectStart(ev any, step events.Postprocessingstep) {
	GinkgoHelper()
	Expect(ev).To(BeAssignableToTypeOf(events.StartPostprocessingStep{}))
	Expect(ev.(events.StartPostprocessingStep).StepToStart).To(Equal(step))
}

func expectFinished(ev any, outcome events.PostprocessingOutcome) {
	GinkgoHelper()
	Expect(ev).To(BeAssignableToTypeOf(events.PostprocessingFinished{}))
	Expect(ev.(events.PostprocessingFinished).Outcome).To(Equal(outcome))
}

var _ = Describe("Postprocessing state machine", func() {
	var cfg config.Postprocessing

	BeforeEach(func() {
		cfg = config.Postprocessing{
			MaxRetries:           2,
			RetryBackoffDuration: time.Second,
			Delayprocessing:      5 * time.Second,
		}
	})

	Describe("Init", func() {
		It("TC-PP-01 finishes with continue when no steps are configured", Label("TC-PP-01"), func() {
			pp := newPP(cfg)

			ev := pp.Init(events.BytesReceived{})

			expectFinished(ev, events.PPOutcomeContinue)
			Expect(pp.Status.CurrentStep).To(Equal(finished))
			Expect(pp.Status.Outcome).To(Equal(events.PPOutcomeContinue))
		})

		It("TC-PP-02 starts the only step when exactly one is configured", Label("TC-PP-02"), func() {
			pp := newPP(cfg, virusscan)

			expectStart(pp.Init(events.BytesReceived{}), virusscan)
			Expect(pp.Status.CurrentStep).To(Equal(virusscan))
		})

		It("TC-PP-03 starts the first of several steps and copies the upload data", Label("TC-PP-03"), func() {
			pp := newPP(cfg, policies, virusscan, delay)

			ev := pp.Init(events.BytesReceived{})

			Expect(ev).To(Equal(events.StartPostprocessingStep{
				UploadID:          "upload-1",
				URL:               "https://example.test/upload-1",
				ExecutingUser:     executingUser,
				Filename:          "report.pdf",
				Filesize:          4096,
				ResourceID:        resourceID,
				StepToStart:       policies,
				ImpersonatingUser: impersonatingUser,
			}))
			Expect(pp.Status.CurrentStep).To(Equal(policies))
		})
	})

	Describe("NextStep with outcome continue", func() {
		var pp *Postprocessing

		BeforeEach(func() {
			pp = newPP(cfg, policies, virusscan, delay)
			pp.Init(events.BytesReceived{})
		})

		It("TC-PP-04 moves from the first step to the second", Label("TC-PP-04"), func() {
			expectStart(pp.NextStep(finishedStep(policies, events.PPOutcomeContinue)), virusscan)
			Expect(pp.Status.CurrentStep).To(Equal(virusscan))
		})

		It("TC-PP-05 moves from a middle step to the last one", Label("TC-PP-05"), func() {
			pp.NextStep(finishedStep(policies, events.PPOutcomeContinue))

			expectStart(pp.NextStep(finishedStep(virusscan, events.PPOutcomeContinue)), delay)
			Expect(pp.Status.CurrentStep).To(Equal(delay))
		})

		It("TC-PP-06 finishes with continue after the last step and copies the upload data", Label("TC-PP-06"), func() {
			pp.NextStep(finishedStep(policies, events.PPOutcomeContinue))
			pp.NextStep(finishedStep(virusscan, events.PPOutcomeContinue))

			ev := pp.NextStep(finishedStep(delay, events.PPOutcomeContinue))

			Expect(ev).To(Equal(events.PostprocessingFinished{
				UploadID:          "upload-1",
				ExecutingUser:     executingUser,
				Filename:          "report.pdf",
				ResourceID:        resourceID,
				Outcome:           events.PPOutcomeContinue,
				ImpersonatingUser: impersonatingUser,
			}))
			Expect(pp.Status.CurrentStep).To(Equal(finished))
			Expect(pp.Status.Outcome).To(Equal(events.PPOutcomeContinue))
		})

		It("TC-PP-07 finishes right away when the only step continues", Label("TC-PP-07"), func() {
			single := newPP(cfg, virusscan)
			single.Init(events.BytesReceived{})

			expectFinished(single.NextStep(finishedStep(virusscan, events.PPOutcomeContinue)), events.PPOutcomeContinue)
		})
	})

	Describe("NextStep decision table: outcome x retries left", func() {
		var pp *Postprocessing

		BeforeEach(func() {
			pp = newPP(cfg, policies, virusscan, delay)
			pp.Init(events.BytesReceived{})
			pp.NextStep(finishedStep(policies, events.PPOutcomeContinue)) // now at virusscan
		})

		It("TC-PP-08 abort finishes the postprocessing and skips the remaining steps", Label("TC-PP-08"), func() {
			expectFinished(pp.NextStep(finishedStep(virusscan, events.PPOutcomeAbort)), events.PPOutcomeAbort)
			Expect(pp.Status.CurrentStep).To(Equal(finished))
			Expect(pp.Failures).To(Equal(0))
		})

		It("TC-PP-09 delete finishes the postprocessing with delete", Label("TC-PP-09"), func() {
			expectFinished(pp.NextStep(finishedStep(virusscan, events.PPOutcomeDelete)), events.PPOutcomeDelete)
			Expect(pp.Status.Outcome).To(Equal(events.PPOutcomeDelete))
		})

		It("TC-PP-10 retry with retries left asks for a retry of the same step", Label("TC-PP-10"), func() {
			ev := pp.NextStep(finishedStep(virusscan, events.PPOutcomeRetry))

			Expect(ev).To(Equal(events.PostprocessingRetry{
				UploadID:        "upload-1",
				ExecutingUser:   executingUser,
				Filename:        "report.pdf",
				Failures:        1,
				BackoffDuration: time.Second,
			}))
			Expect(pp.Failures).To(Equal(1))
			Expect(pp.Status.CurrentStep).To(Equal(virusscan))
			Expect(pp.Status.Outcome).To(Equal(events.PPOutcomeRetry))
		})

		It("TC-PP-11 retry still retries when failures reach max retries exactly", Label("TC-PP-11"), func() {
			pp.NextStep(finishedStep(virusscan, events.PPOutcomeRetry)) // failures = 1

			ev := pp.NextStep(finishedStep(virusscan, events.PPOutcomeRetry)) // failures = 2 = MaxRetries

			Expect(ev).To(BeAssignableToTypeOf(events.PostprocessingRetry{}))
			Expect(ev.(events.PostprocessingRetry).Failures).To(Equal(2))
			Expect(ev.(events.PostprocessingRetry).BackoffDuration).To(Equal(2 * time.Second))
		})

		It("TC-PP-12 retry aborts once failures exceed max retries", Label("TC-PP-12"), func() {
			pp.NextStep(finishedStep(virusscan, events.PPOutcomeRetry))
			pp.NextStep(finishedStep(virusscan, events.PPOutcomeRetry))

			ev := pp.NextStep(finishedStep(virusscan, events.PPOutcomeRetry)) // failures = 3

			expectFinished(ev, events.PPOutcomeAbort)
			Expect(pp.Failures).To(Equal(3))
			Expect(pp.Status.CurrentStep).To(Equal(finished))
		})

		It("TC-PP-13 with max retries 0 the first retry already aborts", Label("TC-PP-13"), func() {
			cfg.MaxRetries = 0
			noRetries := newPP(cfg, virusscan)
			noRetries.Init(events.BytesReceived{})

			expectFinished(noRetries.NextStep(finishedStep(virusscan, events.PPOutcomeRetry)), events.PPOutcomeAbort)
		})

		It("TC-PP-14 continue after a retry moves on and keeps the failure count", Label("TC-PP-14"), func() {
			pp.NextStep(finishedStep(virusscan, events.PPOutcomeRetry))

			expectStart(pp.NextStep(finishedStep(virusscan, events.PPOutcomeContinue)), delay)
			// Assumption (code only): failures are counted per upload, not per step.
			Expect(pp.Failures).To(Equal(1))
		})

		It("TC-PP-15 an unknown outcome finishes the postprocessing with that outcome", Label("TC-PP-15"), func() {
			unknown := events.PostprocessingOutcome("bogus")

			expectFinished(pp.NextStep(finishedStep(virusscan, unknown)), unknown)
			Expect(pp.Status.CurrentStep).To(Equal(finished))
		})
	})

	Describe("CurrentStep (resume)", func() {
		It("TC-PP-16 re-emits the start of the step in progress", Label("TC-PP-16"), func() {
			pp := newPP(cfg, policies, virusscan)
			pp.Init(events.BytesReceived{})
			pp.NextStep(finishedStep(policies, events.PPOutcomeContinue))

			expectStart(pp.CurrentStep(), virusscan)
		})

		It("TC-PP-17 re-emits the same finished event once finished", Label("TC-PP-17"), func() {
			pp := newPP(cfg, virusscan)
			pp.Init(events.BytesReceived{})
			first := pp.NextStep(finishedStep(virusscan, events.PPOutcomeDelete))

			Expect(pp.CurrentStep()).To(Equal(first))
			Expect(pp.CurrentStep()).To(Equal(first))
		})

		It("TC-PP-18 re-emits the step that is waiting for a retry", Label("TC-PP-18"), func() {
			pp := newPP(cfg, virusscan, delay)
			pp.Init(events.BytesReceived{})
			pp.NextStep(finishedStep(virusscan, events.PPOutcomeRetry))

			expectStart(pp.CurrentStep(), virusscan)
		})
	})

	// The following cases are characterization tests: the specification says nothing about
	// these inputs, so they pin down the observed behavior. See the Findings section.
	Describe("NextStep with events that do not match the current state", func() {
		It("TC-PP-19 a finished event for a step that is not configured finishes with continue", Label("TC-PP-19", "characterization"), func() {
			pp := newPP(cfg, policies, virusscan, delay)
			pp.Init(events.BytesReceived{}) // at policies, virusscan has not run

			ev := pp.NextStep(finishedStep(events.Postprocessingstep("thumbnails"), events.PPOutcomeContinue))

			expectFinished(ev, events.PPOutcomeContinue)
		})

		It("TC-PP-20 a stale finished event for an earlier step restarts the following step", Label("TC-PP-20", "characterization"), func() {
			pp := newPP(cfg, policies, virusscan, delay)
			pp.Init(events.BytesReceived{})
			pp.NextStep(finishedStep(policies, events.PPOutcomeContinue))
			pp.NextStep(finishedStep(virusscan, events.PPOutcomeContinue)) // at delay

			expectStart(pp.NextStep(finishedStep(policies, events.PPOutcomeContinue)), virusscan)
			Expect(pp.Status.CurrentStep).To(Equal(virusscan))
		})

		It("TC-PP-21 a finished event that arrives after the end finishes again", Label("TC-PP-21", "characterization"), func() {
			pp := newPP(cfg, virusscan)
			pp.Init(events.BytesReceived{})
			pp.NextStep(finishedStep(virusscan, events.PPOutcomeAbort))

			expectFinished(pp.NextStep(finishedStep(virusscan, events.PPOutcomeContinue)), events.PPOutcomeContinue)
			Expect(pp.Status.Outcome).To(Equal(events.PPOutcomeContinue))
		})

		It("TC-PP-22 a step configured twice never lets the postprocessing finish", Label("TC-PP-22", "characterization"), func() {
			pp := newPP(cfg, virusscan, virusscan)
			pp.Init(events.BytesReceived{})

			for range 5 {
				expectStart(pp.NextStep(finishedStep(virusscan, events.PPOutcomeContinue)), virusscan)
			}
		})
	})

	Describe("BackoffDuration", func() {
		DescribeTable("exponential backoff by number of failures",
			func(base time.Duration, failures int, expected time.Duration) {
				Expect(BackoffDuration(base, failures)).To(Equal(expected))
			},
			Entry("TC-PP-23 negative failures wait nothing", Label("TC-PP-23"), time.Second, -1, time.Duration(0)),
			Entry("TC-PP-24 zero failures wait nothing", Label("TC-PP-24"), time.Second, 0, time.Duration(0)),
			Entry("TC-PP-25 the first failure waits the base", Label("TC-PP-25"), time.Second, 1, time.Second),
			Entry("TC-PP-26 the second failure waits twice the base", Label("TC-PP-26"), time.Second, 2, 2*time.Second),
			Entry("TC-PP-27 the default max retries waits 2^13 times the base", Label("TC-PP-27"), time.Second, 14, 8192*time.Second),
			Entry("TC-PP-28 a zero base never waits", Label("TC-PP-28"), time.Duration(0), 5, time.Duration(0)),
		)

		It("TC-PP-29 keeps growing for a large number of failures", Label("TC-PP-29", "characterization"), func() {
			// Contract (doc comment): every failure waits twice as long as the previous one.
			// Observed: base * 2^(n-1) overflows int64 nanoseconds and wraps around.
			previous := BackoffDuration(time.Second, 34)
			Expect(previous).To(BeNumerically(">", 0))

			Expect(BackoffDuration(time.Second, 35)).NotTo(BeNumerically(">", previous))
		})

		It("TC-PP-30 the method uses the configured base and the current failures", Label("TC-PP-30"), func() {
			cfg.RetryBackoffDuration = 3 * time.Second
			pp := newPP(cfg, virusscan)
			pp.Failures = 3

			Expect(pp.BackoffDuration()).To(Equal(12 * time.Second))
		})
	})

	Describe("Delay", func() {
		var received chan any

		BeforeEach(func() {
			received = make(chan any, 1)
		})

		callback := func(next any) { received <- next }

		It("TC-PP-31 waits the configured time on the clock before handing over the next step", Label("TC-PP-31"), func() {
			clock := newFakeClock()
			pp := newPP(cfg, delay, virusscan).WithClock(clock)
			pp.Init(events.BytesReceived{})

			pp.Delay(callback)

			Eventually(clock.requested).Should(Receive(Equal(5 * time.Second)))
			Consistently(received, 50*time.Millisecond).ShouldNot(Receive())
			// Assumption (code only): the state moves on when Delay is called, not when it ends.
			Expect(pp.Status.CurrentStep).To(Equal(virusscan))

			close(clock.release)
			var next any
			Eventually(received).Should(Receive(&next))
			expectStart(next, virusscan)
		})

		It("TC-PP-32 hands over the finished event when delay is the last step", Label("TC-PP-32"), func() {
			clock := newFakeClock()
			close(clock.release)
			pp := newPP(cfg, virusscan, delay).WithClock(clock)

			pp.Delay(callback)

			var next any
			Eventually(received).Should(Receive(&next))
			expectFinished(next, events.PPOutcomeContinue)
		})

		It("TC-PP-33 finishes with continue when delay is not a configured step", Label("TC-PP-33"), func() {
			clock := newFakeClock()
			close(clock.release)
			pp := newPP(cfg, virusscan).WithClock(clock)

			pp.Delay(callback)

			var next any
			Eventually(received).Should(Receive(&next))
			expectFinished(next, events.PPOutcomeContinue)
		})

		It("TC-PP-34 falls back to the real clock when none was set", Label("TC-PP-34"), func() {
			cfg.Delayprocessing = 0
			pp := &Postprocessing{Steps: []events.Postprocessingstep{delay}, config: cfg}

			pp.Delay(callback)

			var next any
			Eventually(received).Should(Receive(&next))
			expectFinished(next, events.PPOutcomeContinue)
		})
	})
})
