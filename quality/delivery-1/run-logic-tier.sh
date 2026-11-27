#!/usr/bin/env bash
# Logic tier (Go) of Delivery 1: runs the component suites, one verdict per test case.
# Every suite runs with the race detector and in random order. Specs labeled "race" document
# a data race in the production code (finding F7) and therefore run without the detector.
set -uo pipefail
cd "$(dirname "$0")/../.."

out=quality/delivery-1
status=0

run() {
  echo "=== $* ==="
  go test "$@" || status=1
}

# Postprocessing state machine (TC-PP-xx)
run ./services/postprocessing/pkg/postprocessing/ -count=1 -race \
  -coverprofile="$out/postprocessing.cov" \
  -args -ginkgo.v -ginkgo.randomize-all

# Postprocessing service (TC-PS-xx)
run ./services/postprocessing/pkg/service/ -count=1 -race \
  -coverprofile="$out/service.cov" \
  -args -ginkgo.v -ginkgo.randomize-all -ginkgo.label-filter='!race'
run ./services/postprocessing/pkg/service/ -count=1 \
  -args -ginkgo.v -ginkgo.label-filter='race'

echo "=== statement coverage ==="
go tool cover -func="$out/postprocessing.cov"
go tool cover -func="$out/service.cov"

exit $status
