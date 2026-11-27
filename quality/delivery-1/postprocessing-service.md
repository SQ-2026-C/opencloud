# Component: Postprocessing service (event handler)

| Tier | Kind | Location | Dependencies |
|---|---|---|---|
| Backend (Go) | Event handler (API-like: consumes and produces bus events) | `services/postprocessing/pkg/service/service.go` | NATS JetStream publisher (`events.Publisher`), JetStream message (ack / in-progress), nats kv store (`go-micro store.Store`), wall clock (`time.Now`, `time.After`, `time.Sleep`), postprocessing state machine (`pkg/postprocessing`), Prometheus metrics, OpenTelemetry tracer |

**Reason for selection (risk).** This is the only consumer of the postprocessing events. It
decides, for every event, whether JetStream may forget it (ack) or must redeliver it, and it
crashes the whole service when the bus cannot be reached. A wrong ack loses an upload (it
stays *processing* forever); a wrong crash takes postprocessing down for every user. The
publish retry with backoff was changed recently (`publish_max_retries`, introduced in 7.5.0),
so it is also the part with the most recent change.

**Scope.** `PostprocessingService`: `handleEvent` / `processEvent`, `publishWithRetry`,
`getPP`, the resume handling and the worker loop (`Run` / `Close`). The state machine it calls
(`postprocessing.Postprocessing`) is a separate component (see
`postprocessing-state-machine.md`) and is used for real: it is pure logic with no I/O.
`NewPostprocessingService` and `monitorMetrics` connect to NATS and are out of scope (see
Hard to test).

## Contracts

Sources: **[C]** code comments in `service.go`, **[Cfg]** config `desc` tags
(`pkg/config/config.go`), **[N]** NATS JetStream semantics (unacked → redelivered after
`ack_wait`), **[A]** taken only from the code → *assumption*.

### Preconditions
- P1 Each event arrives with its JetStream message, which can be acked or marked in progress. [N]
- P2 `PublishMaxRetries ≥ 0`, `RetryBackoffDuration ≥ 0`, `AckWait ≥ 0`. [Cfg] — not validated. [A]
- P3 An upload is stored under its upload ID as one JSON record. [A]

### Postconditions
- Q1 `BytesReceived` → the upload is stored with the configured steps and `StartTime = now`,
  and the first event of the state machine is published. [A]
- Q2 The source event is acked once it has been handled, *unless* the upload could not be
  stored or the next event could not be published; then it is not acked so JetStream
  redelivers it. [C "Don't ack the source event so jetstream redelivers it"]
- Q3 A failed publish is retried up to `PublishMaxRetries` times; before retry *k* the service
  waits `BackoffDuration(base, k)`, capped at `AckWait/2`, and marks the source event in
  progress. [Cfg publish_max_retries, C publishWithRetry]
- Q4 Retries exhausted → `ErrFatal` (process exits); if the service is stopping or the context is
  cancelled → `ErrEvent` instead. [C "we are shutting down anyway"]
- Q5 `PostprocessingStepFinished` with outcome *retry* → `PostprocessingRetry` is published at
  once; the same step is started again after `pp.BackoffDuration()`. [A]
- Q6 `UploadReady` failed → the upload is kept and marked finished; succeeded → it is deleted
  from the store. Both increment `finished{status}` and observe the duration since
  `StartTime`. [C "keep it around for a while" / "no need to keep it any more"]
- Q7 `ResumePostprocessing{UploadID}` re-publishes the current step of an unfinished upload,
  publishes `RestartPostprocessing` for an unknown one and does nothing for a finished one;
  with a `Step` it resumes every stored upload at that step. [A]
- Q8 `Run` processes events with `Workers` goroutines until the channel is closed or `Close` is
  called. [C]

### Invariants
- I1 Every handled event is acked at most once. [A]
- I2 Between two publish attempts the service never waits longer than `AckWait/2`. [C]

### Exceptional behavior
- E1 Upload not in the store / store read error → `ErrEvent` (logged, worker continues). [A]
- E2 Store write fails → `ErrEvent`, event not acked. [C]
- E3 Store delete fails → `ErrEvent`. [A]
- E4 A failing `Ack` or `InProgress` is only logged. [C "best effort"]
- E5 Store record count ≠ 1 or invalid JSON → error. [A]

## Decision table: ack and error class (Q2, Q4)

| # | Store write | Publish | Stopping / ctx cancelled | Ack? | Result | TC |
|---|---|---|---|---|---|---|
| 1 | ok | ok first time | – | yes | nil | TC-PS-01 |
| 2 | **fails** | not attempted | – | **no** | `ErrEvent` | TC-PS-02 |
| 3 | ok | fails, then ok within retries | no | yes | nil | TC-PS-03 |
| 4 | ok | fails `1+PublishMaxRetries` times | no | **no** | `ErrFatal` | TC-PS-04, 07 |
| 5 | ok | fails | stopping | **no** | `ErrEvent` | TC-PS-05, 21 |
| 6 | ok | fails | ctx cancelled | **no** | `ErrEvent` | TC-PS-06 |
| 7 | (read fails) | not attempted | – | **yes** | `ErrEvent` | TC-PS-10, 11 (F5) |

## Test cases

Techniques: **DT** decision table, **ST** state transition, **EP** equivalence partitioning,
**BVA** boundary value analysis, **EX** exceptional behavior, **CH** characterization (spec silent
or code contradicts the spec). Fixture unless stated: steps `[virusscan]`, production defaults
`RetryBackoffDuration=5s`, `MaxRetries=14`, `PublishMaxRetries=5`, `AckWait=1min`.

| ID | Condition | Technique | Inputs | Expected |
|---|---|---|---|---|
| TC-PS-01 | Q1, Q2 | DT #1 | `BytesReceived`, healthy doubles | stored at virusscan, `StartTime` = fake now, 1 publish `Start(virusscan)`, 1 ack, 0 in-progress |
| TC-PS-02 | Q2, E2 | DT #2, EX | store write fails | `ErrEvent`, 0 publishes, 0 acks |
| TC-PS-03 | Q2, Q3 | DT #3 | publish fails 2× | 3 attempts, 1 ack, 2 in-progress |
| TC-PS-04 | Q4 | DT #4, EX | publish always fails | `ErrFatal`, 6 attempts, 0 acks, 5 in-progress |
| TC-PS-05 | Q4 | DT #5, EX | service closed, clock held | `ErrEvent`, 1 attempt, 0 acks |
| TC-PS-06 | Q4 | DT #6, EX | context cancelled, clock held | `ErrEvent`, 1 attempt, 0 acks |
| TC-PS-07 | Q3, Q4 | DT #4, BVA (retries 0) | `PublishMaxRetries=0`, always fails | `ErrFatal`, 1 attempt, no wait, 0 in-progress |
| TC-PS-08 | E4 | EX | `InProgress` fails, publish fails once | nil, 2 attempts, 1 ack |
| TC-PS-09 | E4 | EX | `Ack` fails | nil |
| TC-PS-10 | E1 | DT #7, CH | step finished for unknown upload | `ErrEvent`, **acked** (F5) |
| TC-PS-11 | E1 | DT #7, CH | store read fails | `ErrEvent`, **acked**, nothing published (F5) |
| TC-PS-12 | adapter | EX | `processEvent(raw.Event)` without message | nil, 1 publish |
| TC-PS-13 | Q3 | EP (below cap) | 3 failures | waits `[5s, 10s, 20s]` |
| TC-PS-14 | Q3, I2 | EP (above cap) | 5 failures | waits `[5s, 10s, 20s, 30s, 30s]` |
| TC-PS-15 | I2 | BVA (= cap) | base 10s, AckWait 20s, 1 failure | waits `[10s]` |
| TC-PS-16 | Q3 | BVA (AckWait 0) | 5 failures | waits `[5s … 80s]`, no cap |
| TC-PS-17 | I2 | BVA, CH | AckWait 1ns (cap rounds to 0) | waits `[5s, 10s]`, no cap (F8) |
| TC-PS-18 | I2 | BVA (smallest cap) | AckWait 2ns | waits `[1ns, 1ns]` |
| TC-PS-19 | Q3 | BVA | 2 retries, always fails | exactly 2 waits, none after the last attempt |
| TC-PS-20 | I2, E | CH, BVA | `PublishMaxRetries=32`, always fails | wait 31 = 30s, wait 32 **negative**, not capped (F4) |
| TC-PS-21 | Q4 | ST (waiting → stopped) | `Close` during a held wait | handler returns `ErrEvent` after 1 attempt |
| TC-PS-22 | – | EP | step finished, empty upload ID | nil, acked, nothing published |
| TC-PS-23 | – | ST | `continue` on last step | `PostprocessingFinished`, stored at finished |
| TC-PS-24 | Q5 | ST | `retry`, clock held | `Retry{5s}` at once; `Start(virusscan)` only after release; failures 1 |
| TC-PS-25 | – | EP | start of a non-delay step | nil, acked, nothing published |
| TC-PS-26 | – | EP | `UploadReady` empty upload ID | nil |
| TC-PS-27 | E1 | EX | `UploadReady` unknown upload | `ErrEvent` |
| TC-PS-28 | Q6 | EP | failed, 42s after start | stored finished, `finished{failed}` +1, duration +42s |
| TC-PS-29 | Q6 | EP | succeeded, 7s after start | delete called, `finished{succeeded}` +1, duration +7s |
| TC-PS-30 | Q6 | CH | succeeded, then resume | upload **back in the store** unfinished; resume re-publishes `Finished` (F6) |
| TC-PS-31 | E3 | EX, CH | delete fails | `ErrEvent`, acked |
| TC-PS-32 | Q7 | EP | resume in-progress upload | `Start(virusscan)` |
| TC-PS-33 | Q7 | EP | resume finished upload | nothing published |
| TC-PS-34 | Q7 | EP | resume unknown upload | `RestartPostprocessing{id}` |
| TC-PS-35 | Q7 | EP | resume by step, 2 at step + 1 finished | starts exactly the 2 |
| TC-PS-36 | Q7 | EX | resume by step, list fails | nothing published |
| TC-PS-37 | E1 | EP | read → not found | `ErrNotFound` |
| TC-PS-38 | E1 | EP | read → other error | that error |
| TC-PS-39 | E5 | BVA (0 records) | | `ErrNotFound` |
| TC-PS-40 | P3 | BVA (1 record) | | decoded upload |
| TC-PS-41 | E5 | BVA (2 records) | | "expected only one result" |
| TC-PS-42 | E5 | EX | invalid JSON | `*json.SyntaxError` |
| TC-PS-43 | Q8 | ST (running → done) | 2 events, channel closed | `Run` returns nil, 2 publishes |
| TC-PS-44 | Q8 | ST (running → stopped) | 2 workers, open channel, `Close` | `Run` returns nil |
| TC-PS-45 | Q8 | EX | `Close` twice | no panic |
| TC-PS-46 | – | ST | start of `delay` step, delay 0 | next step published (F7: data race under `-race`) |
| TC-PS-47 | E1 | EX | `delay` step, unknown upload | `ErrEvent` |
| TC-PS-48 | default clock | EX | no clock set | real clock used (`StartTime ≥ now`) |

## Isolation

| Dependency | Double | Kind | Notes |
|---|---|---|---|
| JetStream publisher (`events.Publisher`) | `testPublisher` | stub + spy | fails the first *n* publishes with `nats: timeout`, records accepted events |
| JetStream message (`Ack`, `InProgress`) | `spyMsg` | spy (+ stub errors) | counts acks / in-progress marks, can fail either |
| nats kv store (`store.Store`) | `stubStore` | fake + stub + spy | map keyed by upload ID; can fail read / write / delete / list or return 0, 2 or invalid records; records deleted keys |
| Wall clock | `fakeClock` | fake | fixed `Now` moved by `Advance`; `After` records the requested wait and fires at once, or is held until `Release` |
| Metrics (Prometheus) | none (real collectors) | – | process wide; tests compare before/after values |
| Tracer | `noop.NewTracerProvider()` | dummy | |
| Logger | `log.NopLogger()` | dummy | |
| State machine (`pkg/postprocessing`) | none (real) | – | separate component, pure logic, tested on its own |

**Changes to production code** (behavior unchanged, defaults are the real implementations):
1. *Message seam.* `raw.Event` (vendored reva) keeps its JetStream message in an unexported
   field, so a test could not observe acks at all: `Ack()` returned "cannot ack event without
   message" and the error was only logged. An `acker` interface (`Ack`, `InProgress`) was
   added; `processEvent(raw.Event)` now forwards to `handleEvent(events.Event, acker)`, and
   `*raw.Event` satisfies `acker`. TC-PS-12 covers the adapter.
2. *Clock seam.* `time.Now`, `time.Since`, `time.After` and `time.Sleep` were replaced by a
   `Clock` interface (`Now`, `After`) with a `realClock` default; `NewPostprocessingService`
   sets it and a nil clock falls back to it (TC-PS-48).

**Nondeterminism.**
- Clock: fully controlled by the fake. The fixture uses the production defaults (5s, 1min) and
  no spec waits on wall-clock time. The former timing assertions (`elapsed ≥ 30ms`) are now
  exact wait sequences.
- Goroutines (step retry, `Delay`, `Run` workers, a handler blocked in a wait): synchronized
  through the held clock and Gomega `Eventually` / `Consistently`, never sleep-then-assert.
- Order of execution: every spec builds its own doubles in `BeforeEach`; metrics are compared
  as deltas. The suite passes 10/10 runs with `-race` and `-ginkgo.randomize-all` with random
  seeds (except TC-PS-46, see F7).
- Random values: upload and event IDs are random UUIDs; no assertion depends on their value.

**Assumptions the doubles make about the real dependencies.**
- Publisher: a failed publish returns an error and publishes nothing (no partial publish).
- Message: `Ack` / `InProgress` are independent calls that can fail without side effects.
- Store: `Read` of a missing key returns `store.ErrNotFound`; one key holds one record;
  `List` returns every key. Notably **go-micro's memory store does not satisfy the last one on
  Windows** (keys built with `filepath.Join`, `List` filters on `"/"`), so it was not used as
  the fake.
- Clock: `After(d)` fires once, after at least `d`; a negative `d` fires at once.

## Findings (to confirm / discuss)

- **F4 (shared with the state machine) — the overflowing backoff also reaches the publish
  retry and escapes the ack-wait cap** (TC-PS-20). With the default 5s base the 32nd backoff
  is negative; `backoff > maxBackoff` is false, so it is not capped and `time.After` fires at
  once. Reachable with `publish_max_retries ≥ 32` (expected: wait 30s; observed: no wait).
- **F5 — a store *read* error drops the event** (TC-PS-10/11). A failed store write leaves the
  event unacked so it is redelivered (Q2), but a failed read returns `ErrEvent` and the event is
  still acked. A transient kv outage while a `PostprocessingStepFinished` is handled leaves
  that upload stuck in processing. Expected (by analogy with Q2): not acked; observed: acked.
- **F6 — a successfully finished upload is written back to the store** (TC-PS-30). After
  `Delete` the `switch` falls through to `storePP`, so the record is recreated (unfinished).
  A later `ResumePostprocessing` for it re-publishes `PostprocessingFinished`. Expected: record
  gone; observed: record present.
- **F7 — data race on `ctx`** (TC-PS-46, run with `-race -ginkgo.label-filter=race`). The
  goroutines started by `pp.Delay` and by the step retry capture the variable `ctx`, which the
  handler reassigns afterwards (`ctx = ctxpkg.ContextSetInitiator(...)`). The race detector
  reports 4 races. A one-line fix (copy `ctx` before starting the goroutine) is possible; the
  code was left as is.
- **F8 — a tiny ack wait disables the cap** (TC-PS-17). `AckWait/2` is integer division, so
  `AckWait = 1ns` gives a cap of 0, which means "no cap". Unrealistic configuration;
  specification problem rather than defect.
- **Hard to test:** `NewPostprocessingService` and `monitorMetrics` (create NATS connections
  directly, no seam); the fatal branches of `Run` (`log.Fatal` calls `os.Exit`); the wait inside
  `Delay` from the service, because uploads read from the store carry no clock.

## Adequacy

- Statement coverage (`go test -cover`): **77.3 %** of the package; `publishWithRetry`,
  `getPP`, `processEvent`, `Close`, `getSteps` at 100 %, `handleEvent` 97.6 %.
  Not executed: `NewPostprocessingService` and `monitorMetrics` (need NATS, out of scope);
  `Run`'s fatal branches (`os.Exit`); `realClock.After` (production only); the
  `json.Marshal` error in `storePP` (unreachable: every field is marshalable); the per-record
  error branches in `findUploadsByStep` and the publish error of `resumePP` (reachable with the
  stub, not yet covered).
- Branch coverage: run `gobco -branch ./services/postprocessing/pkg/service/` — *pending*.
- Mutation testing: run `gremlins unleash ./services/postprocessing/pkg/service` — *pending*.
  Expected equivalent mutant: `backoff > maxBackoff` → `>=` (TC-PS-15 cannot tell them apart,
  both yield the cap).
