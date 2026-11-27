# Component: Postprocessing state machine

| Tier | Kind | Location | Dependencies |
|---|---|---|---|
| Backend (Go) | Logic | `services/postprocessing/pkg/postprocessing/postprocessing.go` | `config.Postprocessing` (value), reva `events` types (values), CS3 `user`/`provider` messages (values), clock (`time.Sleep`) |

**Reason for selection (risk).** Every upload from every client stays in *processing* until this
state machine reaches `finished`. It decides whether a file is released (`continue`), kept
(`abort`) or deleted (`delete`), so a wrong transition either blocks uploads or releases files
that were never scanned. It holds the only retry/abort logic of the pipeline.

**Scope.** The `Postprocessing` type only. Its caller (`pkg/service/service.go`) reads events
from NATS JetStream, keeps the state in a store and publishes the returned events; it is outside
this component. The component itself makes **no network, gRPC or store calls**: it receives an
event value and returns the next event value. Its imports are `math`, `time`, the config struct
and *message/data types* (CS3 protobuf structs, reva event structs) — no clients.

## Contracts

Sources: **[D]** reva doc comments on the event constants (`vendor/.../reva/v3/pkg/events/postprocessing.go`),
**[C]** doc comments in `postprocessing.go`, **[Cfg]** config `desc` tags (`pkg/config/config.go`),
**[A]** taken only from the code → *assumption*.

### Preconditions
- P1 `Steps` holds the configured steps, in order, before `Init` is called. [A]
- P2 `config.MaxRetries ≥ 0`. [Cfg: "maximum number of retries"] — negative is not rejected anywhere. [A]
- P3 Step names in `Steps` are unique. [A] (not enforced — see F3)
- P4 `NextStep` receives the `PostprocessingStepFinished` of the step in progress. [A] (not checked — see F1)

### Postconditions
- Q1 `Init`: no steps → `PostprocessingFinished{continue}`; otherwise `StartPostprocessingStep(Steps[0])`. [C "first step"; empty case A]
- Q2 `NextStep(continue)` on step *i* → start step *i+1*; on the last step → `PostprocessingFinished{continue}`. [D "continue … moved to its final destination"; ordering A]
- Q3 `NextStep(retry)` increments `Failures`; if `Failures ≤ MaxRetries` → `PostprocessingRetry{Failures, BackoffDuration}` and the current step is kept; otherwise → `PostprocessingFinished{abort}`. [Cfg max_retries, D "retry … at a later point"; `>` vs `≥` A]
- Q4 `NextStep(abort|delete|other)` → `PostprocessingFinished{outcome}`. [D for abort/delete; *other* A]
- Q5 `CurrentStep` re-emits the start of the step in progress, or the finished event if finished. [A]
- Q6 `BackoffDuration(base, n)` = 0 for `n < 1`, else `base·2^(n-1)`. [C]
- Q7 `Delay(f)` waits `Delayprocessing` and then calls `f` with the event that follows the `delay` step. [C]
- Q8 Every emitted event carries the upload's ID, user, impersonating user, filename and resource ID (start events also URL and size). [A]

### Invariants
- I1 `Status.CurrentStep ∈ Steps ∪ {finished}` after `Init`. [A]
- I2 `Failures` never decreases. [A]
- I3 Once `finished`, `CurrentStep()` returns the same finished event every time. [A]
- I4 `Status.Outcome == retry` ⇔ the last emitted event was a `PostprocessingRetry`. [A]

### Exceptional behavior
- E1 Retries exhausted → `abort`, not an error. [Cfg + A]
- E2 Unknown outcome → finished with that outcome. [A]
- E3 Finished event for a step that is not configured → finished with `continue`. [A] (F1)
- E4 `failures < 1` → backoff 0; very large `failures` → overflow. [C + A] (F4)
- E5 `delay` not configured → `Delay` hands over `finished{continue}`. [A]

## State model

States: `Init`, `S1…Sn` (step in progress), `R(Si)` (step waiting for retry), `F(o)` (finished with outcome o).

| From \ Event | continue(Si) | retry, failures+1 ≤ max | retry, failures+1 > max | abort / delete / other |
|---|---|---|---|---|
| `Init` (`Init()`) | → S1, or F(continue) if n = 0 | – | – | – |
| `Si`, i < n | → Si+1 | → R(Si) | → F(abort) | → F(o) |
| `Sn` | → F(continue) | → R(Sn) | → F(abort) | → F(o) |
| `R(Si)` | → Si+1 / F(continue) | → R(Si) | → F(abort) | → F(o) |
| `F(o)` | *invalid* → re-finishes (TC-PP-21) | – | – | – |

## Test cases

Techniques: **ST** state transition, **DT** decision table, **EP** equivalence partitioning,
**BVA** boundary value analysis, **EX** exceptional behavior, **CH** characterization (spec silent).
Fixture unless stated: steps `[policies, virusscan, delay]`, `MaxRetries=2`, base backoff 1 s, delay 5 s.

| ID | Condition | Technique | Inputs | Expected |
|---|---|---|---|---|
| TC-PP-01 | Q1, no steps | EP, BVA (len 0) | steps `[]`, `Init` | `Finished{continue}`, state F(continue) |
| TC-PP-02 | Q1, one step | BVA (len 1) | `[virusscan]`, `Init` | `Start(virusscan)` |
| TC-PP-03 | Q1, Q8 | EP (len n) | fixture, `Init` | `Start(policies)` with all upload fields |
| TC-PP-04 | Q2 first→second | ST | `continue(policies)` | `Start(virusscan)` |
| TC-PP-05 | Q2 middle→last | ST | `continue(virusscan)` | `Start(delay)` |
| TC-PP-06 | Q2 last→F, Q8 | ST | `continue(delay)` | `Finished{continue}` with all fields |
| TC-PP-07 | Q2 single step | ST, BVA | `[virusscan]`, `continue` | `Finished{continue}` |
| TC-PP-08 | Q4 abort | DT, ST | at virusscan, `abort` | `Finished{abort}`, failures 0 |
| TC-PP-09 | Q4 delete | DT, ST | `delete` | `Finished{delete}` |
| TC-PP-10 | Q3 retries left | DT, ST | `retry`, failures 0→1 | `Retry{failures 1, 1 s}`, step kept |
| TC-PP-11 | Q3 failures = max | DT, BVA | 2nd `retry` | `Retry{failures 2, 2 s}` |
| TC-PP-12 | Q3/E1 failures = max+1 | DT, BVA, EX | 3rd `retry` | `Finished{abort}` |
| TC-PP-13 | Q3 max = 0 | BVA | `MaxRetries=0`, `retry` | `Finished{abort}` |
| TC-PP-14 | ST R→S, I2 | ST (1-switch) | `retry` then `continue` | `Start(delay)`, failures stays 1 |
| TC-PP-15 | E2 | DT, EX | outcome `"bogus"` | `Finished{"bogus"}` |
| TC-PP-16 | Q5 in progress | ST | `CurrentStep()` at virusscan | `Start(virusscan)` |
| TC-PP-17 | Q5, I3 | ST | `CurrentStep()` ×2 after delete | same `Finished{delete}` |
| TC-PP-18 | Q5 after retry | ST | `CurrentStep()` in R(virusscan) | `Start(virusscan)` |
| TC-PP-19 | E3, P4 | CH, EX | at policies, `continue("thumbnails")` | observed `Finished{continue}` (F1) |
| TC-PP-20 | P4 stale event | CH, ST | at delay, `continue(policies)` | observed `Start(virusscan)` again (F1) |
| TC-PP-21 | event after F | CH, ST (invalid) | after abort, `continue(virusscan)` | observed `Finished{continue}`, outcome overwritten (F1) |
| TC-PP-22 | P3 duplicates | CH, EX | `[virusscan, virusscan]`, `continue` ×5 | observed `Start(virusscan)` every time (F3) |
| TC-PP-23 | Q6 | EP, BVA | n = −1 | 0 |
| TC-PP-24 | Q6 | BVA | n = 0 | 0 |
| TC-PP-25 | Q6 | BVA | n = 1 | base |
| TC-PP-26 | Q6 | EP | n = 2 | 2·base |
| TC-PP-27 | Q6 | BVA (default max) | n = 14 | 8192·base |
| TC-PP-28 | Q6 | EP | base = 0, n = 5 | 0 |
| TC-PP-29 | Q6/E4 | CH, BVA | base 1 s, n = 34 vs 35 | observed n = 35 not larger than n = 34 (F4) |
| TC-PP-30 | Q6 method | EP | base 3 s, failures 3 | 12 s |
| TC-PP-31 | Q7 | ST, EX | fake clock, `[delay, virusscan]` | sleeps 5 s, no callback before release, then `Start(virusscan)` |
| TC-PP-32 | Q7 last step | ST | `[virusscan, delay]` | callback gets `Finished{continue}` |
| TC-PP-33 | E5 | EX | `[virusscan]` | callback gets `Finished{continue}` |
| TC-PP-34 | default clock | EX | struct-literal instance, delay 0 | real clock used, callback runs |

## Isolation

| Dependency | Double | Notes |
|---|---|---|
| `config.Postprocessing` | none (real value, built per test) | plain struct, no behavior |
| reva `events` structs, CS3 `user`/`provider` | none (real values / fixtures) | data types only |
| Clock (`time.Sleep` in `Delay`) | **fake** `fakeClock` injected with `WithClock` | records the requested duration and blocks until the test releases it |

**Change to production code.** `Delay` called `time.Sleep` directly, so it could only be tested
with real waiting. A `Clock` interface (`Sleep(d)`) was added with `WithClock`; when no clock is
set (struct literal in `service.go`, JSON unmarshal from the store) it falls back to the real one,
so runtime behavior is unchanged. TC-PP-34 covers that fallback.

**Nondeterminism.**
- Clock: controlled by the fake; no test depends on wall-clock time.
- Goroutine in `Delay`: synchronized through channels (`requested`, `release`, callback channel)
  with Gomega `Eventually`/`Consistently`; no `sleep`-then-assert.
- Order of execution: every spec builds its own instance in `BeforeEach`/inline; no shared state.
  The suite passes with `-race` and with Ginkgo `--randomize-all`.
- Random values: none used.

**Assumptions the doubles make.** The real clock's `Sleep` returns after *at least* `d` and never
fails; `Delay` is fine with `Sleep` running in its own goroutine.

## Findings (to confirm / discuss)

- **F1 — `NextStep` never checks that the finished step is the one in progress** (TC-PP-19/20/21).
  Observed: an event for an unconfigured step finishes the upload with `continue`; a stale event
  re-runs a later step; an event after `finished` overwrites the outcome. *Specification problem:*
  nothing states what should happen. At component level the behavior is certain; whether it can
  happen in the system depends on JetStream delivery (at-least-once → redeliveries are possible) and
  on configuration changes between restarts. That is integration scope and is **not** claimed here.
- **F2 — `Failures` is never reset between steps** (TC-PP-14). `max_retries` is documented as
  "for a failed postprocessing step", but the counter is per upload. Specification vs. code mismatch.
- **F3 — duplicate step names make the pipeline loop forever** (TC-PP-22). No validation in the parser.
- **F4 — `BackoffDuration` overflows** for base 1 s at n ≥ 35 (TC-PP-29), contradicting its doc
  comment. Not reachable with the default `MaxRetries=14`, reachable if it is configured ≥ 35.
  The service reuses `BackoffDuration` for publish retries, where it is reachable with
  `publish_max_retries ≥ 32` at the default 5s base and escapes the ack-wait cap (see F4 and
  TC-PS-20 in `postprocessing-service.md`).
- **Hard to test:** `Delay` (hard-wired clock + goroutine) — resolved with the clock seam.

## Adequacy

- Statement coverage (`go test -cover`): **100 %**. Go reports statements, not branches.
- Branch coverage: run `gobco -branch ./services/postprocessing/pkg/postprocessing/` — *pending*.
- Mutation testing: run `gremlins unleash ./services/postprocessing/pkg/postprocessing` — *pending*.
