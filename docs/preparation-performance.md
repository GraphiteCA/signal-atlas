# Preparation performance and locking

This follows Phase 4 commit `53c6d07`. Observation storage remains a measured
choice; faster preparation is the primary objective. The reference solver
remains the default, and existing survey settings and completed work are kept.

## Changes

Preparation calculations feed a continuous, bounded worker pool. A worker takes
another target immediately after finishing; a slow target no longer blocks an
entire wave. Fresh settings use up to four workers, and Admin permits 1–16.
Previously saved worker counts are preserved. The working-memory allowance is
still shared between workers, so higher concurrency lowers the per-target
allowance; large targets can require a higher budget or fewer workers.

Workers release metadata connections before the CPU solver. One coordinator
commits completed output at most 64 targets or approximately every 100 ms, with
a 2 MiB queued-output flush threshold. An individual target remains atomic even
when larger than that threshold. Computed results remain bounded by the worker
queue, batch limits and existing per-target scratch estimate.

Each accepted target's estimates, checkpoint and pending-work removal share a
transaction. The input version is checked again inside the transaction. Stale
or already-completed targets are skipped individually. Notifications are sent
only after commit. Cancellation discards uncommitted output; completed durable
checkpoints remain and pending targets are retried after resume. A crash may
repeat the most recent uncommitted calculations, without losing input scans.

Clustered map reads pause dispatch briefly instead of repeatedly cancelling
ongoing calculations. Foreground writes, progressive unclustered map loading,
manual jobs, switching, pause and shutdown still cancel and join preparation.

Metadata write transactions use `BEGIN IMMEDIATE` before reading versions or
counters. Read-only map snapshots retain deferred `BEGIN`. Map preferences and
comparison writes now share the Atlas writer gate. Failed commits and cancelled
transactions roll back before returning the connection, temporarily disabling
interruption only for rollback cleanup. Lock waits use the driver's
context-aware busy handler, so pause/shutdown can interrupt contention promptly
instead of waiting through SQLite's sleeping busy-timeout handler. Preparation
failures are logged with the extended SQLite result code. External processes can still hold a database
lock; timeouts and cancellation remain necessary operational behavior.

Both solvers now select coarse-grid starts with a stable ordered heap rather
than sorting every candidate. The grid, score calculation, eight-start spacing
rule, candidate order and tie-breaking are retained. This is a search-order
implementation improvement, not a change of model or numerical accuracy.

## Full-sweep benchmark method

`cmd/fullprepbench` starts from an owned, unfitted workspace, performs actual
preparation until every target has a durable checkpoint, and reports successful
locations separately from unavailable outcomes. No synthetic saved locations
or pre-completed checkpoints are accepted. All input files and source workspaces
are preserved; metadata copies are independent and immutable segments may share
disk blocks.

The large fixture has 10M source scans, 9,999,900 accepted observations and
199,998 surviving preparation targets (two radios lose all their zero-pair rows).
The before build is frozen from Phase 4. A full
large reference diagnostic was initially attempted but deliberately stopped and
retained as a partial diagnostic because its rate was about 20 targets/s.
It is excluded from complete-sweep comparisons.

The complete large runs use **the same existing fast solver, sample mode and
shared grouping**. Solver choice is explicit. The before/default profile uses
two workers; optimized native and DuckDB profiles use the same tuned worker
count and eight Go scheduler slots. Worker scaling is part of the before/after
result and must not be attributed entirely to batching or storage. A separate
complete sweep of the original scan imports uses the reference solver and two
workers before and after, to retain a matched reference-workload comparison.

The DuckDB prototype reads observations for 256 targets at a time on a dedicated
connection, ordered by canonical source ID. It builds bounded read-only fitting
views, feeds continuously running Go workers, and uses the **same production
grouping, sampler, solver and SQLite batch checkpoint writer**. It isolates
observation-storage/read differences; it is not a production DuckDB migration
or a replacement metadata engine.

Canonical DuckDB fixture setup copies typed values and original IDs from an
owned native snapshot through the DuckDB appender. This avoids CSV parser,
normalization or parallel row-number differences affecting fit inputs. Conversion
time is reported separately from preparation, and row counts are checked.
Views cannot be committed as persistent observation segments. Source IDs are
preserved through sampling, including sparse ID subsets.

Result checks include full checkpoint counts, saved fitted/unavailable counts,
and hashes of sorted estimate identities, scopes and JSON results. Reopen and
map responsiveness are checked separately. Working-set measurements cover the
whole process, including native DuckDB memory. DuckDB uses a 1 GB buffer-manager
setting; this is not a hard process memory limit. Filesystem cache is uncontrolled.
No compilers, test suites or other benchmark workloads run alongside measured
processes.

## Complete-sweep results

Windows amd64, Intel Core i7-10700K (8 cores / 16 logical processors), Go 1.27.3,
eight Go scheduler slots. Each row is one complete unfitted sweep, not an
extrapolation from a short sample. These are single trials with uncontrolled
filesystem cache, rather than a confidence interval.

| Dataset and solver | Build / observations backend | Workers | Complete targets | Fitted | Unavailable | Elapsed | Targets/s |
| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: |
| 10M source rows, fast | Phase 4 native | 2 | 199,998 | 199,998 | 0 | 40m 05.75s | 83.13 |
| Same large sweep | Optimized native | 8 | 199,998 | 199,998 | 0 | 7m 15.89s | 458.82 |
| Same large sweep | DuckDB, batched reads | 8 | 199,998 | 199,998 | 0 | 8m 52.78s | 375.39 |
| 175,447 real observations, reference | Phase 4 native | 2 | 39,066 | 7,141 | 31,925 | 5m 52.29s | 110.89 |
| Same real sweep | Optimized native | 2 | 39,066 | 7,141 | 31,925 | 2m 08.17s | 304.80 |

The large default-to-tuned comparison improves **5.52×**, including concurrency
tuning. The matched two-worker reference comparison improves **2.75×**.
Unavailable outcomes are durable checkpoints, not successful locations. The
real runs save 13,137 estimate records, including explicit unavailable results;
targets with no eligible groups need only their checkpoint.

Every large saved identity/scope/result matches exactly across all three builds:
SHA-256 `03aa11865278c70bc4d097bc2eca454f5412a9d2e91001df649a6d32ed645b63`.
The two reference runs also match exactly:
`0f0fe0e29d2241f8a695e01f6a5477841849036499204d9d6e6879b0dadb13e0`.

Native saves 199,998 target results in 4,270 batches. Its summed writer stage is
103.07 seconds and summed worker stages 2,807.06 seconds. DuckDB uses 782 read
batches (147.92 seconds summed), 4,545 commits (120.03 seconds) and 2,897.03
seconds summed worker stages. These stages overlap and worker times include
scheduling; they are not additive wall time or a CPU profile. Canonical DuckDB
fixture conversion takes 32.69 seconds separately from preparation.

Optimized native peaks at **2.25 GiB** process working set; DuckDB at **1.09 GiB**.
The old harness opened the native columns once for validation and again for the
measured sweep, which can inflate its 4.42 GiB process peak. Its preparation
timer includes one normal measured open, but its memory figure is not a clean
before/after RAM comparison. The two optimized harnesses validate metadata
without that extra native column load. Neither figure includes the WebView.

## Storage recommendation

Keep the current immutable column store plus SQLite metadata for this release.
The complete batched-read DuckDB prototype takes **22.2% longer** than tuned
native preparation. DuckDB provides a substantial memory benefit, but it does
not provide the requested additional preparation-speed improvement here.
The locking fix belongs to metadata transaction ownership and cancellation;
changing the observation backend would leave those SQLite writes in place in
this prototype.

This does not establish a limit on DuckDB. The adapter uses canonical ID-ordered
observations and batched joins, without an additional preparation-specific
sorted projection. Such a projection, different read batches or a fully
streaming production map backend need separate measurement and parity work.
No existing survey is migrated and no production DuckDB dependency is added.

Use Admin's concurrent-calculation control to reproduce the tuned eight-worker
profile. Existing survey worker settings are preserved; the reference solver
remains the default. Increasing workers shares the existing scratch allowance
more thinly, so watch the explicit limited/pending status for unusually large
SSIDs. The matched reference result shows a substantial improvement even with
the previous two-worker setting.

Raw commands, timings, samples, process peaks and binary/result hashes are
committed in [preparation-performance.json](../benchmarks/baselines/preparation-performance.json).
Owned full databases and raw logs remain under
`build/benchmarks/preparation-perf-ib58v4ph`; they are not committed.

After full preparation, the 10M-row workspace reopens to complete status in
4.16 seconds without computing or committing a target, and preserves the saved
hash. On 200 changing viewport requests, clustered map p95 is 11.68 ms; repeated
cached clustered responses have p95 2.00 ms. The first map index build is still
4.62 seconds. These are engine timings, not end-to-end frame measurements.
The tested viewport contains 163,631 calculated APs; the complete database holds
199,998 calculated locations. The first-index cost remains a useful follow-up
performance target.

## Validation and limitations

The full Go suite includes Python numerical parity. Additional tests reproduce a
WAL read-to-write upgrade failure, verify immediate reservation and prompt lock
cancellation, and reuse a connection after cancelled rollback. Batch tests cover
stale input versions, checkpoint replay and all-or-nothing failure. Pool tests
force a slow worker and cross the 64-result boundary. An early optimized
diagnostic exposed a batch-limit guard error before measurement; it was stopped,
retained separately, fixed and excluded from final results.

The read-view test uses sparse source IDs and more than 256 observations to
exercise the actual sampler. It requires identical fit output to the persistent
store. The heap-order test compares original stable sorting, ties and NaNs.
Focused race checks cover preparation, worker lifecycle and transaction changes.

All five completed metadata databases pass SQLite quick-check, with zero pending
targets and zero stale checkpoint versions. The generated 10M-row input and all
nine original scan CSV hashes are unchanged. The complete Go suite, Python
numerical parity, vet, focused race suites, eight JavaScript map checks and four
benchmark safety tests pass; the separate DuckDB module also builds and vets.

Real Wails/WebView2 delivery is checked with browser polling disabled: all 64
freshly calculated APs appear on the GPU, native event delivery is 32–35 ms and
first visibility is 125 ms after commit. The browser polling fallback also
loads all 64 without panning, with first visibility at 415 ms. Pause and
survey-specific all-observation/both-grouping settings survive reload. These
small native/browser checks validate actual delivery and controls separately
from the full-dataset headless throughput run.

Run the full-sweep orchestration after building the two probes:

```powershell
$env:GOTOOLCHAIN='go1.27.3'
go build -trimpath -o build/benchmarks/fullprep-after.exe ./cmd/fullprepbench
go build -trimpath -o build/benchmarks/mapbench-preparation.exe ./cmd/mapbench
# In tools/duckbench, with CGO_ENABLED=1:
# go build -trimpath -o ../../build/benchmarks/duckprep.exe ./preparation
python benchmarks/preparation_perf.py --root build/benchmarks/preparation-perf-ib58v4ph
# After building the desktop app and nativebench-preparation.exe:
python benchmarks/preparation_checks.py --root build/benchmarks/preparation-perf-ib58v4ph
```

The script resumes successful stages, requires fresh independent metadata for
each sweep, and refuses an unowned benchmark root. Frozen before measurements
are retained under the same root. Completed-sweep reopen verification runs in a
separate process, checks that no targets are calculated again and compares the
saved-result hash. Its startup time is not treated as preparation throughput.
