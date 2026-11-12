# Memory and preparation follow-up

> Historical implementation and measurement report. Claims about defaults,
> validation and upstream revisions refer to the work recorded here. For current
> behavior and checks, use the [documentation index](README.md).

Base: `0b73cab` (Phase 5), branch `codex/memory-and-preparation`.

## Recommendation

Adopt a **bounded DuckDB observation provider** as the next storage phase, while
retaining the Go solvers, GPU map and compact SQLite location/checkpoint metadata.
The tuned prototype completes the actual large survey within about 8% of the
revised resident engine and uses 84% less peak engine working set. This supersedes
the earlier conclusion that the measured DuckDB preparation/query gap justified
retaining fully resident observations indefinitely.

This commit delivers immediate improvements to the existing app and a measured
DuckDB adapter. It does **not** automatically convert surveys or make DuckDB a
production storage option. Production observation reads still use immutable Go
columns, with SQLite metadata. The independent Drive archive is unchanged.

A fivefold improvement for the *entire* preparation sweep has not been demonstrated.
The map-active cohort improved 2.6 times; dense-history sampling improved much
more. Changing observation storage alone does not eliminate checkpoint/result
writing, which still accounts for much of the complete sweep.

## Actual workload and measurement boundaries

The private survey contains 5,753,026 accepted scans across 117 segments/imports,
5,649,215 interned strings, 2,304,010 radio groups and 1,460,561 preparation targets.
That cardinality is much higher than the earlier 10M-row / 200K-radio fixture.
A target is a named SSID or one hidden-SSID MAC; targets and fitted AP locations
are different counts. All figures below use the existing **fast / sample / shared**
settings with float64 GPU scoring, unless explicitly stated otherwise.

Inputs were copied using a read-only SQLite backup, independent dictionaries and
immutable observation links. No import, clearing, migration or source-database
write was performed on the active survey. Preparation was paused by the user
before the final comparisons. Earlier exploratory runs while the app remained
active are retained locally but are excluded from the primary comparisons.

Host: Windows, i7-10700K, 8 physical/16 logical cores, 32 GB RAM, RTX 2080 Ti,
driver 621.01. Go 1.27.3; DuckDB Go driver v2.20000.0 / DuckDB 2.0. Go scheduler
budget 8, eight preparation workers; DuckDB queries use two native threads.
Working sets are sampled every 50 ms, including native allocations. Headless
engine memory excludes WebView and dedicated GPU VRAM. Filesystem cache is
uncontrolled. End-to-end sweep timings are individual capacity runs, not a
statistically established latency distribution. No compilation or other benchmark
ran alongside a primary timing trial.

The 97,370-target cohort selects every fifteenth original catalog row. It retains
**all 5.75M scans and original observation IDs**. It is not a complete-survey
sweep. Full runs below process all 1,460,561 targets from an unfitted copy.

## Results

| Workload | Implementation | Seconds | Peak engine working set |
|---|---|---:|---:|
| Cohort, map requests every 400 ms | Committed Phase 5 | 58.912 | 4.40 GiB |
| Same cohort and map requests | Final native | 22.339 | 2.89 GiB |
| Cohort, map requests every 50 ms | Final native | 22.389 | 2.87 GiB |
| Complete survey, before final writer/scheduling changes | Intermediate native | 572.622 | 3.05 GiB |
| Complete survey | Final native | 339.292 | 3.03 GiB |
| Complete survey, same solver and SQLite save policy | Tuned DuckDB provider | 366.215 | 0.473 GiB |

The intermediate full run already includes dictionary/map memory work and bounded
sampling; its 9m33s is **not** a measured stock Phase 5 full-survey baseline.
The final full run takes 5m39s; DuckDB takes 6m06s. Process lifetime, including
report hashing/close, is recorded separately in the JSON evidence.

Both complete runs produce **170,765 fitted locations**, **1,289,796 unavailable
outcomes**, **338,826 saved estimate JSON rows**, and exactly **1,460,561 durable
checkpoints**, with zero pending or blocked targets. Some unavailable outcomes
have saved diagnostic JSON; others have no eligible group and only a checkpoint.
The native full rate is about 4,305 completed targets/s or 503 fitted locations/s.
All 170,765 numerical fits used the physical GPU, with zero scoring fallbacks.

All complete variants have the same sorted `(radio_key, scope_key, result)` SHA256:
`ebe35e7920bce842a4119d49032416f26e07c289ffad6170ba2924413b7aa914`.
The cohort hash is
`0acf515409e7747b9263da653cc47ef01a8101ac0e38909a306c5dfef303af2f`.
These compare original IDs, sampling, numerical results, scopes and provenance;
counts alone are not the equivalence test.

The actual `FitRadio` sampling pilot uses 200,000 rows, three radios and one
receiver position, so model validation exits before numerical grid scoring.
Against the committed production implementation it improves **318.47 ms to
4.87 ms per call (65 times)**, with identical complete result JSON. Allocation
falls from **89,230,688 to 334,724 bytes/call**. This isolates a dense-history
bottleneck; it is not a claim of a 65-fold complete-survey speedup.

## Changes in the app

- Replace millions of temporary per-radio maps in map eligibility with typed
  counts and a compact sorted slice. Single-radio groups avoid general alias/
  hashing work; shared-only groups above twenty radios cannot produce a group.
- Store dictionary reverse lookups as a compact ID hash table, with full string
  equality checks. Immutable byte arenas replace millions of tiny string objects
  on reopen. IDs, file format and import identities are preserved.
- Use bounded heaps for the original strongest-32 and lowest-hash-224 samples per
  radio. Preserve ordering, ID tie breakers, date/count metadata, small-input
  behavior and the full-observation solver. Sample scratch no longer grows with
  an individual AP's entire scan history.
- Return an unavailable checkpoint directly for raw single-radio targets below
  four scans in shared-only mode. Multi-radio insufficient groups still retain
  their original diagnostic JSON. Stop shared grouping as soon as a filtered
  twenty-first radio makes that group ineligible.
- Read past in-flight queue claims instead of repeatedly fetching the same head.
  Seed pending indexes in key order. Save up to 512 targets / 2 MiB, or on the
  existing 100 ms flush tick; publish updates only after commit.
- Validate staged batch versions with joins, then insert checkpoints and dequeue
  in sets. Stale targets, blocked targets, cancellation, replay and atomic
  estimate/checkpoint publication retain their contracts.
- Use a dedicated, cancellable cache writer so prepared statements remain warm.
  Only this writer uses WAL `synchronous=NORMAL`; import/admin connections stay
  `FULL`. Estimates, typed locations and checkpoints remain one transaction.
  Recent derived cache work can require recalculation after power/OS failure;
  ordinary application crashes retain transactional results. SQLite documents
  these distinctions in its [synchronous pragma](https://www.sqlite.org/pragma.html#pragma_synchronous).
- Materialize valid locations and distinct-name counters in small typed SQLite
  tables. Map reads avoid repeatedly decoding estimate JSON. Map revisions now
  change when locations change, so unsuccessful fits do not reload the map.
  Existing saved estimates are backfilled once; scan files are not rewritten.
- Remove the post-map 100 ms cooldown for clustered AP reads. WAL snapshots and
  immutable input state allow these reads alongside preparation. Large sighting
  buffers still yield dispatch; foreground writes, paginated reads and handoffs
  retain cancellation/join coordination.
- Default new preparation preferences to at most eight workers, while preserving
  saved preferences. Admin continues to allow 1–16 workers.
- Show APs in view, clusters and individual APs. Remove the unclear “map symbols”
  wording and the loading suffix on routine background refreshes. Keep initial
  loading and progressive page counts, stale-response guards and retained buffers.

The memory probe's forced-GC stages distinguish live data from temporary garbage;
production does not force GC, trim working sets or hide memory with mmap accounting.
The original live-process observation was roughly 4.90 GiB in the Go process and
5.37 GiB across its family; controlled engine comparisons are listed separately.

## DuckDB tuning that changed the outcome

The prior preparation query joined a temporary target list through an SSID/hidden
MAC `OR` condition against unordered observations. On this high-cardinality survey,
the exploratory unordered path was stopped after minutes without finishing its
first batch; it is a failed diagnostic, not a complete timing result.

The new candidate physically orders all original columns by a persisted
`target_key, id`, uses 16,384-row groups, a prepared bounded-range/equality join,
one reused read connection, and explicit ID ordering before the Go sampler.
There is no ART observation index. It streams the target catalog in bounded
pages and uses 512-target observation views, two DuckDB threads, a 256 MB buffer
budget and an owned spill directory during physical ordering. Process RAM is
measured independently of that buffer setting.

These choices follow DuckDB's guidance on [ordered data and zonemaps](https://duckdb.org/docs/2.0/guides/performance/indexing),
[connection reuse, prepared statements, row groups and thread counts](https://duckdb.org/docs/2.0/guides/performance/how_to_tune_workloads),
and [memory limits and allocations outside the buffer manager](https://duckdb.org/docs/2.0/guides/performance/oom).
ART indexes can retain significant memory and were not added speculatively.
Eight Go workers do not imply eight DuckDB query threads; four query threads did
not improve the exploratory cohort. The 256 MB cohort used 445 MiB process RAM
and was at least as quick as the exploratory 512 MB setup.

Canonical conversion and physical ordering are separate setup costs: about 24.3s
and 9.9s in exploratory runs, respectively; these are not included in preparation
rates or presented as uncontended import benchmarks. Every original column and
ID is retained, including nullable accuracy/frequency and source record/line data.
The complete result hash checks the numerical read contract. This adapter still
uses row scanning into bounded views; Arrow transfer remains an unimplemented
candidate, rather than an assumed speedup.

## Desktop, query and correctness validation

The real Wails/WebView2 GPU page completed a populated-view trace during actual
preparation: 900 frames, **16.8 ms p95**, **3.7 ms pause/join**, zero empty AP frames
and zero flashing loading frames. It loaded 479,407 sightings across the survey;
the anchored z15 trace had 317 local sightings and one AP, so this is not presented
as a new dense-marker rendering benchmark. Its peak simultaneous process-family
working set was 3.41 GiB. Earlier Phase 5 dense synthetic rendering evidence remains
separate. The initial fixed-coordinate pilot was excluded because that view was
nearly empty in this survey.

A completed full native copy reopened in **6.50s**, retained all checkpoints and
result hashes, and performed **zero GPU scoring calls**. Target-aware DuckDB
lookups returning all 17 columns measured **8.20 ms p95** over 200 requests with
two threads and a 512 MB buffer limit. Each timed read follows an identity query
that warms the target row group; these are not cold-disk results or a fresh native
point-query comparison. Ordered/canonical all-column row fingerprints match.

The full Go suite, active Python numerical parity checks, vet and targeted race
checks pass. Eleven JavaScript display/controller tests, seven Python benchmark
safety tests and the separate DuckDB module checks pass. Both full metadata copies
pass SQLite `quick_check`, have no stale-version checkpoints and have no pending
work. Linux amd64 and macOS arm64 shared-engine builds pass with CPU fallback;
this phase's desktop/hardware measurements are Windows only.

## Next implementation phase and acceptance gates

1. Introduce a bounded observation-provider interface for catalogs, target inputs,
   viewport points and SQL reads. Move direct `Obs.State()` consumers behind it.
   Keep the current provider for compatibility and compare both on the same data.
   Include map/catalog filtering: materializing the full survey in those consumers
   would defeat the bounded-memory provider.
2. Port the measured target-ordered DuckDB layout and streaming target reads.
   Preserve all original IDs/fields and exact sampler order. Add an explicit,
   recoverable migration/new-survey storage choice through Admin.
3. Preserve import atomicity across observation data and metadata, including
   interrupted append, source-hash deduplication, selective invalidation, backups,
   database switching and generation recovery. No silent in-place conversion.
4. Keep typed location snapshots/counts fast while scans stay on disk. Benchmark
   viewport point reads separately: target ordering does not establish spatial
   query performance. Never turn a limited render sample into a scan-count claim.
5. Target the remaining sequential metadata cost. The final native sweep spends
   190.84s committing; DuckDB spends 187.74s using the same writer. Benchmark
   stable integer target/namespace IDs, covering queue/version reads, bulk handling
   of provably ineligible targets, and compact versioned checkpoint pages before
   committing to a schema change. Preserve per-target invalidation and the atomic
   relationship between checkpoints and saved locations.
6. Test batched independent-AP GPU grids and reuse of immutable coarse grids only
   after those costs are separated. More workers alone were not a large gain;
   small sparse-group CPU fast runs were competitive with GPU fast runs under the
   earlier save policy. Preserve float64 validation, CPU fallback and solver scopes.

Require complete-sweep raw-result parity, bounded whole-process memory, retained
map responsiveness during live preparation, and import/recovery/admin/SQL-browser
round trips. Rebenchmark imports on fresh sources: the previous phase's import
numbers do not certify the new physical layout. Target a fivefold complete-sweep
improvement against a directly measured stock baseline, rather than multiplying
unrelated microbenchmark gains.

## Reproduction and evidence

Committed sanitized evidence: `benchmarks/baselines/memory-preparation-summary.json`.
Raw private records, profiles, query plans and disposable fixtures remain ignored
under `build/benchmarks/memory-duck-swep7r2i/`.

Build `cmd/fullprepbench`, `cmd/memorybench`, `cmd/samplerbench`, and the separate
`tools/duckbench/preparation` / `inspect` module commands. For stock comparisons,
freeze `0b73cab` and copy only the current benchmark command into that source tree.
The original production implementation remains unchanged in that frozen tree.

```powershell
# Use an owned native snapshot; --stride 1 completes every target.
python benchmarks/memory_preparation.py --source <owned-store> --native <native-probe.exe> --baseline <baseline-probe.exe> --stride 1 --map-poll-ms 0
# Or add --snapshot-source for an explicit read-only backup of a physical survey.
# Add --duck-probe <duck-probe.exe> --ordered-duck <owned-ordered.duckdb> for parity.
```

`cmd/memorybench` requires an owned copy and reports open, live-heap and map stages.
`cmd/samplerbench` is entirely synthetic and isolates sampling through actual
production `FitRadio`. The native renderer driver is `benchmarks/memory-native.js`.
Verification results, query-plan details and new build hashes are recorded in the
sanitized evidence file after the checks complete.
