# Phase 3: automatic AP preparation

Plan items J01–J05 are implemented on Phase 2 commit `5c54f94`. The checked upstream main is still `6d8833659906faa89b1677424caf5f5cd905c56d`.

## Behavior and controls

Preparation is enabled by default. It calculates saved locations across the survey without requiring map movement. Default settings are **representative sample**, **shared AP locations**, up to **two simultaneous calculations** (one on smaller CPUs), and **256 MB estimated calculation scratch space**. The existing app-wide fast-solver preference selects its namespace; **reference remains the default**.

The GPU map shows progress and Pause/Resume. Admin adds a versioned, survey-specific automatic-preparation flag, pause state, sample/full-data mode, shared/individual/both grouping, concurrent-calculation limit (1–4), and working-memory allowance (64–2048 MB). These survive workspace and portable SQLite snapshots. Display controls such as clustering, label zoom and rings do not schedule fits.

Preparation uses the global survey scope: all dates and bands, minimum signal -150 dBm. Other date/signal/filter scopes remain available through the existing selected/nearby/“Fit every AP” workflows. Switching the map's display model does not silently change preparation settings. Full-data fits retain their existing display precedence over sampled results within the selected solver.

The scratch allowance is an estimate based on target observation counts, divided among workers; it is separate from the column store, metadata and WebView memory. Oversized targets remain **pending**, with a visible limited status, rather than being marked complete or silently downgraded. Increase the allowance, reduce concurrency, or calculate those targets manually. Automatic preparation can be disabled without discarding committed work.

## Scheduling and checkpoints

A runner belongs to one survey session. Survey writes cancel/join background work before entering their request lease. Manual background jobs reserve priority until they finish. Map requests receive priority, including a short quiet interval between progressive AP pages, while independent Compare and Drive sessions remain available. The scheduler starts with a short grace period for the first map, caps its workers, and reuses the upstream grouping and solvers. Hidden-MAC grouping now uses the transmitter index instead of repeatedly scanning every hidden observation.

Metadata tracks stable named-SSID and hidden-MAC targets with independent input versions. Preparation namespace identity includes grouping, observation scope, solver and fit mode. Durable pending rows and fitted/unavailable checkpoints are updated transactionally. Pending counters and a ready-work index let completed sweeps reopen without calling the target grouper for every network. The initial catalogue is seeded once from existing observation indexes; a namespace's first queue build is a separate startup cost.

Imports and cleanup retain unaffected versions, fits and checkpoints. Manual band changes now invalidate only the affected named SSID or hidden MAC, replacing the earlier global estimate deletion. Affected targets are versioned and queued in bulk SQL in the same metadata transaction as invalidation. Previously unavailable targets become eligible for another attempt when their inputs change. Historical comparison pins remain untouched. A target with no eligible groups receives a terminal unavailable checkpoint; retired input identities can remain in the tracked-target count.

Workers compute outside the writer lock, then validate the target input version before committing all of that target's results and checkpoint together. A superseded calculation cannot publish a current result or completion marker. Manual fitting also checks target versions before saving. Large sample sorting has internal cancellation boundaries without changing reference sort order. Pause, survey switching, cleanup, snapshots and shutdown cancel/join the relevant runner before closing its Atlas. Switches create a new session/job identity and reject old work and notifications.

## Live delivery and map loading

`JobEventSink` publishes notifications only after committed fit/checkpoint transactions. Manual and selected fit jobs use the same adapter while keeping their legacy responses. Events identify the survey, job, namespace, solver, fit mode, grouping and target versions. The in-memory history holds 128 events; polling returns at most 32 and requests reconciliation if it missed history. Native notifications coalesce at 100 ms with at most 32 target references, and slow sinks cannot block fit workers. The bridge reconciles sequence numbers every 400 ms as a recovery path.

Wails injects its JavaScript runtime automatically on folder/index routes. Our extensionless `/map` and `/admin` pages now explicitly load Wails' built-in IPC/runtime assets before attaching native event handlers. Browser mode omits that bootstrap. The native test blocks polling and verifies that actual Wails events still update the GPU map; the browser test separately checks polling recovery.

A SQLite change journal retains the last 2,048 fit revisions. Live map requests decode only changed fit identities and reuse unaffected typed rows, observation eligibility and other solver namespaces. Gaps, observation changes or a new scope fall back to the complete snapshot path. SQL inserts, updates, deletes, scope moves and rollbacks retain consistent invalidation. Counts use a partial expression index, and observed-name counting is retained across fit-only updates.

Solver preference changes publish a reconciliation event and change the view revision without invalidating observation buffers; solver-independent eligibility stays cached. Observation revision is now separate from fit revision on the client. New saved locations update APs and counts while unchanged multi-million-point sighting buffers stay on the GPU. Live refreshes coalesce without repeatedly cancelling progressive pages.

## Verification and measurements

The full Go suite and Python/NumPy parity pass, along with Go vet, race-detector checks for preparation/session/event/map concurrency, eight JavaScript map tests and four benchmark-safety tests. Tests cover both solvers, both fit modes, all three grouping choices, pause/reopen, completed-sweep reuse, unavailable outcomes, selective dirty work, cleanup, portable snapshots, stale-worker rejection, memory deferral, sort cancellation, survey retirement, post-commit events and slow subscribers. Windows desktop and native probe builds pass.

Owned Phase 0 fixture: **10M observations, 200,000 tracked targets, approximately 50,000 synthetic saved AP locations**. Windows/i7-10700K/32 GB, eight scheduler slots, filesystem cache uncontrolled. No tests or compilers ran concurrently with measured probes. The large probe exercises a **partial** preparation sweep, foreground priority, snapshots and restart/resume; it does not claim an end-to-end time for calculating all 200,000 targets. The separate 64-network UI fixture completes its actual reference calculations.

| Check | Final result |
|---|---:|
| Map API, preparation enabled and foreground priority | 0.87 ms p95 |
| Map API after a forced committed fit update before every request | 50.31 ms p95 |
| Count update after each forced commit | 31.21 ms p95 |
| Pause and join, large survey | 1.57 ms |
| Complete workspace snapshot including pending work | 1.03 s |
| Partial-survey reopen | 5.10 s |
| 10M-row import, including target-version maintenance | 20.56 s; 29.18 M accepted rows/minute |

The forced-commit trace temporarily changes one synthetic saved location and restores it afterward. It measures incremental update work rather than repeated hits on an unchanged response. Native Wails delivery with polling blocked arrived in **75–100 ms**, with the first AP visible in **172 ms**. Browser polling delivered all 64 APs without panning; its maximum observed event delay was **403 ms**, with the first visible AP in **177 ms**. These are single-host probes with raw timings, not universal latency guarantees.

The first per-target SQL implementation imported the fixture in 32.05 s. The final bulk invalidation replaces that path. Two unprofiled bulk runs took 20.46 s and 20.56 s; a separate CPU-profiled repeat took 19.89 s. Selective metadata invalidation/bookkeeping accounted for 2.42 CPU seconds in that profile. Compared with Phase 2's 18.00 s, the remaining roughly 2.5 s is explicit durable-tracking overhead on this fixture. Import counts remain **9,999,900 accepted / 100 zero pairs excluded**, with an unchanged source hash. Working-set peaks include the probe's reopen/snapshot checks and are not a measurement of preparation's incremental memory alone. Native WebView/GPU verification uses the small completed fixture; the large capacity probe measures the engine, not large-survey WebView memory.

Raw measurements, process/binary hashes, smoke results and exclusions are in [phase3-summary.json](../benchmarks/baselines/phase3-summary.json). All tests use temporary or owned benchmark surveys; original inputs and live surveys are unchanged.

```powershell
go test -count=1 ./...
go vet ./...
go test -race ./internal/engine ./internal/atlas -run 'TestAutomatic|TestPreparation|TestAdmin|TestMapJournal'
node --test benchmarks/map-display.test.cjs
python -m unittest discover -s benchmarks -p test_run.py -v
go build -trimpath -o build/benchmarks/preparationbench-phase3.exe ./cmd/preparationbench
go build -trimpath -o build/benchmarks/baseline-phase3.exe ./cmd/baseline
go build -trimpath -tags desktop,production -o build/benchmarks/nativebench-phase3.exe ./cmd/nativebench
go build -trimpath -tags desktop,production -o build/SignalAtlas-phase3.exe .
python benchmarks/phase3.py
python benchmarks/visual.py --data OWNED-BROWSER-DATA --app build/SignalAtlas-phase3.exe --driver preparation-smoke.cjs
```

`phase3.py` creates fresh clones of an owned Phase 0 fixture and fresh paused UI fixtures. Historical benchmark tools explicitly disable automatic preparation so existing manual-fit measurements remain comparable; `nativebench -automatic` enables it for the Phase 3 driver. Full-observation SQL and storage-capacity redesign remain Phase 4 work.
