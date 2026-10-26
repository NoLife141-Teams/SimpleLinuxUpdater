#!/usr/bin/env bash
# Disposable local measurement. No normal application startup, scheduler or SSH.
set -euo pipefail
cd "$(dirname "$0")/../.."
out="${1:?Usage: tools/performance/dashboard.sh /absolute/output-directory}"
case "$out" in /*) ;; *) echo 'Output directory must be absolute' >&2; exit 2;; esac
mkdir -p "$out"
export GOCACHE="${GOCACHE:-/tmp/simplelinuxupdater-analysis-go-cache}"
export GOMAXPROCS="${GOMAXPROCS:-2}"
export SLU_SCALING_OUTPUT="$out"
binary="$out/scaling.test"
trap 'rm -f "$binary"' EXIT
python3 tools/performance/provenance.py > "$out/provenance.json"
go test -p=1 -count=1 -timeout=3m -run '^TestDashboardScaling(Measurements|PhaseMeasurements|SSE)$' . ./internal/observability > "$out/measurements.log" 2>&1
go test -run '^$' -bench '^BenchmarkDashboardScaling$' -benchmem -benchtime=300ms -count=5 . > "$out/bench-http.log" 2>&1
go test -run '^$' -bench '^BenchmarkDashboardScalingPhases$' -benchmem -benchtime=300ms -count=5 ./internal/observability > "$out/bench-phases.log" 2>&1
go test -run '^$' -bench '^BenchmarkDashboardScaling/1000/DashboardHTTP$' -benchtime=3s -cpuprofile="$out/cpu.pprof" -memprofile="$out/heap.pprof" -o "$binary" . > "$out/profile.log" 2>&1
go tool pprof -top "$out/cpu.pprof" > "$out/cpu-top.txt"
go tool pprof -top -alloc_space "$out/heap.pprof" > "$out/alloc-top.txt"
go test -c -o "$binary" .
node tools/performance/dashboard-browser.cjs "$binary" "$out" > "$out/browser.log" 2>&1
for size in 10 100 1000; do node tools/performance/summarize-browser-profile.cjs "$out/cpu-browser-$size.json" > "$out/cpu-browser-$size-summary.json"; done
echo "Measurements complete: $out"
