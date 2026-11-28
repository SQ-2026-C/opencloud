#!/usr/bin/env bash
# Runs every component test suite, one per tier, and prints a verdict per test.
# Usage: ./run-tests.sh            (exit code 1 if any test fails)
#
# Add a suite per tier as:  "tier|component|go package|test-name regex"
SUITES=(
  "data|Role-assignment & permission store|./services/settings/pkg/store/metadata|^TestTC_STORE_"
)

set -u
cd "$(dirname "$0")"
overall=0

for suite in "${SUITES[@]}"; do
  IFS='|' read -r tier component pkg pattern <<<"$suite"
  printf '\n\033[1m== Tier: %s — %s (go test) ==\033[0m\n' "$tier" "$component"

  # -count=1: no cached results · -shuffle=on: random order, seed printed for replay
  go test -json -count=1 -shuffle=on -run "$pattern" "$pkg" | python3 -c '
import json, re, sys
tests, out, seed = {}, {}, None
for line in sys.stdin:
    try: e = json.loads(line)
    except ValueError: continue
    t, a = e.get("Test"), e.get("Action")
    if a == "output":
        m = re.search(r"-test.shuffle (\d+)", e["Output"])
        if m: seed = m.group(1)
        if t: out.setdefault(t.split("/")[0], []).append(e["Output"])
    if t and "/" not in t and a in ("pass", "fail", "skip"):
        tests[t] = a
passed = failed = 0
for t in sorted(tests):
    m = re.match(r"TestTC_([A-Z]+)_(\d+)_(.*)", t)
    tid, name = (f"TC-{m.group(1)}-{m.group(2)}", m.group(3)) if m else ("-", t)
    v = tests[t]
    colour = {"pass": "32", "fail": "31"}.get(v, "33")
    print(f"  \033[{colour}m{v.upper():4}\033[0m  {tid}  {name}")
    if v == "fail":
        failed += 1
        for l in out.get(t, []):
            l = l.strip()
            if l.startswith(("Error:", "Messages:", "expected:", "actual  :")) or "store called" in l:
                print("          " + re.sub(r"\s+", " ", l)[:110])
    elif v == "pass":
        passed += 1
print(f"  shuffle seed: {seed}")
verdict = "\033[32mPASS\033[0m" if failed == 0 else "\033[31mFAIL\033[0m"
print(f"  Verdict: {verdict} — {passed} passed, {failed} failed, {len(tests)} total")
sys.exit(1 if failed else 0)
' || overall=1
done

exit $overall
