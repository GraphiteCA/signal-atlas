# Baseline benchmarking

Phase 0 measures the upstream application at `6d8833659906faa89b1677424caf5f5cd905c56d`. Production engine, UI and storage behavior are unchanged. Benchmark commands are separate programs; they never use the application's normal data directory.

Results and the final phase decision are recorded in `benchmarks/baselines/phase0-summary.json`. Raw local evidence is retained under `build/benchmarks/` and source copies under `build/benchmarks/sources-*/`. Real survey inputs and raw records are not checked into the repository.

## Measurement contract

- Reference host: Windows, Intel Core i7-10700K, 8 physical/16 logical cores, 32 GB installed memory. Controlled engine runs use `GOMAXPROCS=8`, eight fit workers and eight DuckDB workers. Upstream still creates its normal parser/scan goroutines; the scheduler CPU budget is controlled, rather than changing production code.
- The donor uses its existing 2 GB DuckDB working-memory setting. This is **not** a total process-memory cap. Record process working sets, including native allocations, as well as Go heap figures.
- Stop a headless probe above 12 GiB peak working set or 30 minutes. Native renderer probes sample the engine and descendant WebView processes, with a 6 GiB simultaneous family working-set ceiling and 150-second timeout. A stopped run is a capacity failure, not a completed timing trial.
- Filesystem cache is uncontrolled. A separate process measures process-cold reopen; warm request traces are reported separately. These are not cold-disk measurements.
- Import time includes hashing, parsing/validation, persistence, indexing/catalog construction and return from the committed import. Throughput counts newly accepted rows; whole-file duplicates do not count as imported work.
- The current upstream accepts 0,0 CSV rows. Fixtures record their number and the expected future filtered count. Phase 2 changes that policy; do not compare rates with different accepted-row counts without stating the difference.
- Run at least three independent primary import trials. Run 200 requests per representative viewport trace once per fixture. Additional import trials and process-cold reopen can use one request per trace. Capacity latency samples are explicitly smaller and are not used to claim a reliable p95.
- The full donor high-cardinality trace was manually interrupted after about thirteen minutes. Its partial data is excluded; replacement diagnostics use five requests per trace. The completed upstream traces and donor repeated-position trace retain 200-request distributions. This exception is recorded rather than presenting limited samples as equivalent p95 evidence.
- Keep approximate/reference solver choice separate from sample/full-observation input choice. Do not count unavailable or reused outcomes as newly fitted locations.

## Programs

`cmd/baseline` calls the unchanged upstream engine and records JSON import outcomes, queries, both solvers and fit modes, process-cold open, optional SQLite export/reimport, archive round trips and Go memory. Fresh import directories are mandatory. It does not call `RemoveAll`.

`cmd/controlbench` checks 25 small imports, preservation of untouched fits, concurrent viewport requests, cancellation/join and reuse after resume. It creates a new survey for each invocation.

`cmd/nativebench` hosts the existing map pages in Wails/WebView2. `benchmarks/native.js` records data-load/display timing, main-thread draw submission or canvas drawing, and requestAnimationFrame pacing. A separate fixture seeds synthetic saved AP locations with actual map group identities; these are **rendering fixtures, not fitted or validated AP estimates**. The donor canvas runs as a component in the same host against the same AP payload; its result does not measure the donor desktop shell.

Native measurements distinguish GPU AP-only rendering from 0.5M/2M/8M observation limits. The existing explorer may show only a sampled subset of APs; record shown and total separately. Controlled pan traces reuse loaded data: the driver temporarily disables the app's auto-fetch entrypoint while retaining map/renderer event listeners. API request traces are measured separately. RAF pacing is not a GPU timer-query measurement. Process working sets do not measure dedicated GPU VRAM. Capture library/tile delivery and any WebGL or offline errors rather than treating them as successful rendering.

The donor adapter in `benchmarks/donorprobe/main.go` is build-tagged and copied into the frozen donor module before compilation. It measures that store and solver with the same input files and query shapes. Solver algorithms, sampling and fit batching differ, so their results are separate baselines rather than an interchangeable correctness comparison.

## Reproduction

From the repository root, with Go, Python/NumPy and Node available:

```powershell
go build -trimpath -o build/benchmarks/baseline.exe ./cmd/baseline
go build -trimpath -o build/benchmarks/controlbench.exe ./cmd/controlbench
go build -trimpath -tags desktop,production -o build/benchmarks/nativebench.exe ./cmd/nativebench
python -m unittest discover -s benchmarks -p test_run.py -v
go test -count=1 -json ./... > build/benchmarks/tests.jsonl
python benchmarks/run.py freeze --donor ..
python benchmarks/run.py generate --csv build/benchmarks/fixtures/repeated-10m.csv --rows 10000000 --radios 1000
python benchmarks/run.py run --csv build/benchmarks/fixtures/repeated-10m.csv --manifest build/benchmarks/fixtures/repeated-10m.json --probe build/benchmarks/baseline.exe --trials 3 --threads 8 --requests 200 --fits 8
```

Generation refuses to overwrite a fixture. Reuse an existing fixture only after verifying its sidecar SHA-256. Generate the high-cardinality variant with `--radios 200000 --high-cardinality`; use `--rows 50000000 --radios 1000` for capacity. The full workload contract is in `benchmarks/manifests/windows-baseline.json`.

For native probes, use a completed run's owned `trial-1` data directory, never a normal survey. The program checks the benchmark ownership marker. Each page has a separate WebView profile. The native runner creates a new result directory and records renderer diagnostics, executable/script hashes and sampled process-family memory:

```powershell
python benchmarks/run.py native --data build/benchmarks/run-EXAMPLE/trial-1/store --page gpu --probe build/benchmarks/nativebench.exe --donor ../ --seed-fits 50000
```

Repeat with `--page explorer` and `--page donor` on the same prepared render fixture, without reseeding. The `data` path above is the probe's `-store` argument: the directory containing the engine's own `store/` directory. Substitute the actual owned run path.

For profiling, use a separate diagnostic run with `-cpuprofile`/`-memprofile`. Exclude profiled runs from throughput and latency comparisons. Keep the CPU profile and `go tool pprof -top` output beside its raw result.

## Correctness and acceptance

The harness checks imported observation counts, duplicate no-op behavior, complete SQLite snapshot reopen counts, archive CSV deduplication, untouched-fit preservation and resumed-job cache reuse. The upstream suite adds Python/NumPy result parity and Node comparison parity, plus saved-database and logger-reader coverage. Record every failed or skipped test; packages without tests are not skipped test cases.

Provisional goals are 10M newly eligible rows/minute, cached viewport p95 at most 250 ms, a 16 ms overview draw budget, committed-fit visibility within 500 ms and cancellation acknowledgement within 1 second. Distinguish API visibility from the existing UI's polling interval, and draw CPU time from RAF pacing. Unimplemented push delivery is not a passing baseline feature.

A phase conclusion must identify failed budgets and the next profiled work. Capacity failure moves storage evaluation forward. Map latency failure prioritizes the responsible query/render path. Preserve the source/fixture manifests and raw evidence for all later phase comparisons.
