# Metadata preparation follow-up

> Historical implementation and measurement report. Claims about defaults,
> validation and upstream revisions refer to the work recorded here. For current
> behavior and checks, use the [documentation index](README.md).

Base: `e60214a`, branch `codex/preparation-metadata`, 11 November 2026.
See [benchmark evidence](../benchmarks/baselines/metadata-preparation.json) and
the separate [observation-quality recommendation](observation-quality.md).

## Full-survey result

The complete preparation workload contains **5,753,026 scans and 1,460,561
targets**. Every full run starts from an unfitted independent copy, uses eight
workers, eight Go scheduler threads, GPU-fast scoring, sampled inputs and shared
grouping. No original survey or input log was modified.

| Complete run | Elapsed seconds | Time inside batch saves | Peak engine working set |
|---|---:|---:|---:|
| Current committed baseline | 336.004 | 215.754 | 3.086 GiB |
| Compact/bulk metadata plus overlapped writer | 179.504 | 142.575 | 3.148 GiB |
| Larger checkpoint interval | 151.131 | 110.193 | 3.141 GiB |
| Final implementation, confirmation | 148.812 | 110.763 | 3.144 GiB |

The final configuration is about **2.2× faster**, reducing elapsed preparation
time by roughly **55%**. This includes startup/catalog initialization and the
metadata migration needed by the unfitted fixture. Result hashing and process
close are outside the reported preparation interval and recorded separately.
Batch-save durations overlap calculations after the pipeline change; they must
not be added to worker timings to reconstruct elapsed time.

All variants produce 170,765 fitted outcomes, 1,289,796 unavailable outcomes,
338,826 saved estimate JSON rows, and 1,460,561 durable checkpoints. Pending and
blocked counts finish at zero. Every one of the 170,765 successful fits uses
the physical GPU, with zero scoring fallbacks. The sorted complete estimate hash is:

`ebe35e7920bce842a4119d49032416f26e07c289ffad6170ba2924413b7aa914`

Checkpoint identities/versions/outcomes, all typed map-location columns, and
located-network counts also have identical independent hashes. Completion
timestamps and map revision numbers naturally differ with batch boundaries.
SQLite quick checks pass and the transactional batching guard is clear.

The final metadata file is about 1.078 GB versus 1.244 GB before, roughly 13%
smaller for these fresh full preparations. Existing files can reuse freed pages;
this migration does not automatically vacuum or shrink them. Peak process RAM
increased by about 57–59 MiB, including the larger writer cache and overlapped batches.
Observation storage and the GPU scratch limit are unchanged. This is not a DuckDB
migration or a large resident-memory reduction.

## What changed

The full baseline spent about 216 seconds saving batches. The changes target
that serial path and avoid idling calculation workers while it runs:

- Store the small preparation target, pending and checkpoint tables as
  `WITHOUT ROWID`, avoiding a second B-tree containing the same text keys.
- Replace the all-target ready index with a partial index for exceptional
  memory-blocked targets. Ordinary queue reads follow the primary key; eligible
  blocked work still resumes when its budget permits.
- Maintain namespace pending counts once per bulk enqueue/dequeue, and update
  map projections, located-network counts and map-change records in sets.
- Publish one map revision per location batch. The bounded change log discards
  complete revisions so incremental map refreshes never miss half a batch.
- Remove the old JSON-derived location indexes from `estimates`. Legacy viewport
  reads now join the typed location projection; raw estimate JSON and rowids remain
  intact. Manual fits, deletes and ordinary SQL writers retain the trigger path.
- Overlap the single writer with one collecting calculation batch. Dispatch
  stops when that bounded pipeline fills. Claims remain reserved until the writer
  acknowledges the committed transaction; events are emitted after commit.
- Give the dedicated cache writer a 32 MiB page-cache target, in-memory bounded
  staging tables and a checkpoint trigger of 16,384 WAL pages, about 64 MiB at
  this database's 4 KiB page size. The default 1,000-page interval caused frequent
  checkpoint/file work in the full profile.

The table layout follows SQLite's [WITHOUT ROWID guidance](https://www.sqlite.org/withoutrowid.html),
and connection tuning uses its [documented pragmas](https://www.sqlite.org/pragma.html).
The WAL value is a checkpoint threshold, not a hard file-size cap: active read
snapshots can delay reclamation. The cache writer retains `synchronous=NORMAL`
for recomputable results; import/admin connections retain `FULL`. A power loss
can require repeating recent cache work. No journal disabling or unsafe writes
were introduced.

## Consistency and migration

An immediate transaction still validates each target's current input version
against both the target catalog and pending queue. Stale or duplicate results are
handled as before. Estimates, locations, network counts, checkpoints, pending
counts and map revisions commit together.

Bulk operations use a transaction-local change to a persistent guard row to
suppress redundant row triggers. SQLite's single-writer isolation prevents other
connections from writing through or seeing the uncommitted guard. Commit restores
it to zero; rollback or crash restores the previous state. Ordinary writers keep
their existing trigger-maintained behavior. Tests inject a failure after map
projection publication to verify complete rollback and connection reuse.

Layout migration copies the internal preparation tables in one transaction before
the connection pool opens. It preserves input versions, paused/blocked work,
completed checkpoints and saved results. On a completed legacy full-survey copy,
the first open plus migration took **35.17 seconds**, with zero solver calls;
subsequent reopening took **6.47 seconds**. Both retained the complete estimate
hash. The first open of an existing large survey can therefore take noticeably
longer once. Temporary free disk space is needed to copy these metadata tables.

Pause, database switch and shutdown cancel and join workers and the writer.
Viewport priority and the global fairness share remain active. The blocked-writer
test proves calculation overlap and bounded buffering, including partial timer
batches under the race detector.

## Validation and measurement limits

The actual Wails GPU page passed 900 pan frames during real preparation: **16.9 ms
p95**, no empty AP frames, no flashing loading text, no JavaScript errors,
133.5 ms pause/join and 118 viewport-priority dispatches in the release check. The trace anchored an
actual AP at zoom 15 and contained 61,233 sightings and 105 APs at sampling time.
It initially loaded 479,407 survey sightings. This validates responsiveness under
load; it is not an identical scene comparison with the earlier one-AP trace.

The full Go suite, Python numerical parity, targeted race checks, physical GPU
scoring/solver checks, vet, 13 JavaScript controller tests, seven benchmark safety
tests and Linux/macOS core fallback builds pass. Additional checks cover migration,
rollback after projection changes, replacing/removing locations, exact network
counts, oversized map revisions and ordinary manual-write compatibility.

Machine: i7-10700K, 32 GiB, RTX 2080 Ti, driver 621.01, Go 1.27.3, Windows.
Primary preparation runs are sequential, with no competing agent benchmarks or
test suites. Each includes the same CPU-profile flag. Other desktop activity and
filesystem cache state were uncontrolled. There is one baseline, one intermediate
and two final full capacity runs; this is not a statistically established latency
distribution. The small cohort pilot and quality prototype timings are separate
from full-survey results.

Reproduce on an already owned unfitted fixture:

```text
python benchmarks/metadata_perf.py --source-owned <owned-unfitted-store> --base e60214a
```

The runner clones metadata/dictionaries, shares immutable observation segments,
freezes source, builds each revision, measures both full runs and compares complete
result hashes. It never clears or opens an original survey. `cmd/fullprepbench
-verify-existing` verifies a completed copy, and `benchmarks/memory-native.js`
checks the real desktop map. Packaged app: `build/SignalAtlas-metadata.exe`.
