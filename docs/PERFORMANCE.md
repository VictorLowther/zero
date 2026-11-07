# Zero Performance Benchmarks

The performance harness tracks three release-facing signals:

- Cold start: process startup time for `zero --version`.
- Binary first output: time from spawning the built `zero --version` command to
  the first stdout or stderr chunk.
- Harness end memory: RSS for the Go benchmark harness after the spawned
  command exits, plus the delta from the pre-spawn RSS sample.

Cold start uses the built Go binary at `./zero` or `./zero.exe`. Run
`go run ./cmd/zero-release build` before the benchmark so it measures the
production runtime.
On Linux the harness memory metric reads RSS from `/proc/self/statm`; on other
hosts `readHarnessMemoryMb()` falls back to `runtime.ReadMemStats()` and reports
`MemStats.Sys` in MB when process RSS is not available from the standard library.

This smoke benchmark does not measure provider TTFT or Go agent memory. A
provider-aware Go benchmark should be added separately when the runtime exposes a
deterministic local streaming path.

## Run Locally

```bash
go run ./cmd/zero-perf-bench
```

Run against a freshly built binary:

```bash
go run ./cmd/zero-release build
go run ./cmd/zero-perf-bench
```

Write the JSON report used by CI:

```bash
go run ./cmd/zero-perf-bench --output dist/perf/perf-bench.json
```

Default warning thresholds:

- Cold start p95: 300 ms
- Binary first-output p95: 500 ms
- Harness end RSS max: 256 MB

The default sample count is intentionally small for CI smoke coverage. `p95` uses nearest-rank percentile selection, so with the default 5 measured samples it is the slowest sample. Increase `--iterations` for local baseline investigations.

Override thresholds with CLI flags:

```bash
go run ./cmd/zero-perf-bench --cold-start-warn-ms=350 --first-output-warn-ms=600 --harness-end-rss-warn-mb=384
```

Or with environment variables:

```bash
ZERO_PERF_COLD_START_WARN_MS=350 go run ./cmd/zero-perf-bench
```

Supported environment variables:

- `ZERO_PERF_ITERATIONS`
- `ZERO_PERF_WARMUP_ITERATIONS`
- `ZERO_PERF_COLD_START_WARN_MS`
- `ZERO_PERF_FIRST_OUTPUT_WARN_MS`
- `ZERO_PERF_HARNESS_END_RSS_WARN_MB`

## CPU and Memory Profiling

The `zero` CLI accepts process-wide pprof flags that wrap any run — the
interactive TUI, `exec`, or a one-shot prompt:

```bash
zero --cpuprofile cpu.out --memprofile heap.out exec "fix the failing test"
go tool pprof -http=:8080 cpu.out
go tool pprof -http=:8080 heap.out
```

- `--cpuprofile <path>` records a CPU profile for the whole process lifetime.
- `--memprofile <path>` writes a heap profile when the process exits, so it
  reflects the working set the run actually accumulated.
- Both are leading root flags and compose in any order with `--add-dir`,
  `--theme`, and `--allow-escalation`; a path that cannot be opened fails the
  run loudly rather than silently skipping collection.

This is the intended way to profile render hot paths (for example the stream
coalescing in `internal/tui`) against real sessions instead of external
samplers.

## CI Behavior

The `Performance Smoke` job builds the binary, runs
`go run ./cmd/zero-perf-bench --output dist/perf/perf-bench.json --ci`, and
uploads `dist/perf/perf-bench.json`.

Threshold drift is emitted as GitHub Actions warnings. The job fails only if the benchmark cannot run, the build fails, or `--fail-on-warning` is passed explicitly.
