# Phase 4: storage evaluation, lower observation memory and SQL browser

Phase 4 starts at `481057a` (Phase 3), on upstream
`6d8833659906faa89b1677424caf5f5cd905c56d`. The production explorer continues
to use immutable observation columns and SQLite metadata. The independent
Drive archive remains SQLite. DuckDB is an isolated benchmark candidate in
`tools/duckbench`, with no production dependency or automatic conversion.

## Storage decision

Retain one production explorer store: **the optimized column store with SQLite
metadata**, and provide SQL through a read-only virtual-table adapter over its
real observations. For the current interactive/preparation workload, this keeps
fast radio access and imports, preserves the existing GPU caches, and reduces
memory without an observation export or a second persistent copy.

DuckDB is the stronger candidate for compressed disk storage, inexpensive
reopen, small incremental writes and lower resident memory. Those advantages are material. In this
evaluation its radio retrieval was substantially slower, and its import path
does not yet preserve the production preparation/checkpoint and recoverable
workspace contracts. An additional ART-index pilot did not remove the measured
radio-query gap. The evidence supports retaining the current store for this
phase; it does not establish that DuckDB cannot be tuned differently or that
the column store will suit arbitrarily large, memory-constrained surveys.

No survey migration, identity remapping or deduplication change is needed for
this decision. Existing workspaces open in place. Portable SQLite conversion,
complete snapshots, recovery revisions, import source IDs, comparisons,
preferences and selective fit invalidation keep their existing contracts.
The SQL adapter exposes all 17 persisted observation fields, including original
record IDs/line numbers, timestamps, frequency/band source and nullable accuracy.
Capture types or node information belonging to the separate Drive archive are
not silently reinterpreted as explorer observation fields.

## Memory changes

- Each segment now reads directly into its final aligned backing allocation.
  Reopen no longer holds both a file-sized `ReadFile` buffer and an aligned copy.
  Headers, row counts, ID arithmetic and complete column lengths are checked
  before constructing typed slices.
- Radio, transmitter, SSID and grid indexes use two-pass, exact-size contiguous
  posting lists. They retain ascending row order and remove growth slack and
  discarded backing arrays.
- Small append-only imports reuse immutable, unaffected indexes and catalogue
  entries. Only changed posting lists/statistics are copied and merged. Removals,
  band replacements, maintenance and large appends still rebuild completely.
  Randomized tests compare repeated appends against full rebuilds and check that
  previously captured snapshots remain unchanged.
- Small band sets live directly in catalogue statistics, replacing a separate
  map allocation for every transmitter/network.
- CSV parsing respects the available Go scheduler slots, uses a smaller bounded
  queue, and releases raw record/line slices once each batch is parsed. The
  ordered writer, validation, zero-pair policy and reference solver are retained.

There is no working-set trimming, forced swapping or mmap-only accounting
change. Measurements below use total process peak working set, including the
native DuckDB library, rather than just Go heap statistics. The store still
keeps its observation columns and derived indexes resident; it is not a
disk-backed solution for surveys larger than available RAM.

## Matched measurements

Windows amd64, Intel i7-10700K, 32 GB RAM, Intel RAID volume, Go 1.27.3.
Eight Go scheduler slots and eight DuckDB worker threads. DuckDB driver
`v2.20000.0`, buffer-manager limit **1 GB**. That setting is not a process memory
cap; [DuckDB documents memory outside its buffer manager](https://duckdb.org/docs/current/guides/performance/oom).
No compilers or test suites ran alongside measured workloads. Filesystem cache
was uncontrolled; reopen results describe a fresh process, not cold disk.

Each 10M-row input contains 100 zero-coordinate pairs. Both importers accept
**9,999,900 observations**, count the 100 exclusions and hash the same unchanged
source file. Complete imports include hashing, parsing, validation, observation
writes, catalogue/index work and transaction commit. The current app also
maintains its production preparation target versions. Native background fitting
is disabled during controlled storage/map probes. No solver throughput claim is
inferred from a retrieval benchmark.

Medians of three fresh import trials per row; peak RAM is the median of the
three process peaks. The frozen Phase 3 executable is the “before” build.

| Dataset / store | Import seconds | Accepted rows/min | Peak process RAM |
|---|---:|---:|---:|
| 200,000 radios · before | 25.90 | 23.17M | 2.519 GiB |
| 200,000 radios · optimized | 21.31 | 28.16M | 2.135 GiB |
| 200,000 radios · DuckDB | 34.58 | 17.35M | 1.087 GiB |
| 1,000 radios / repeated GPS · before | 16.54 | 36.27M | 1.952 GiB |
| 1,000 radios / repeated GPS · optimized | 14.54 | 41.27M | 1.921 GiB |
| 1,000 radios / repeated GPS · DuckDB | 26.58 | 22.57M | 1.080 GiB |

| Fresh-process reopen | Before | Optimized | Peak RAM before → optimized |
|---|---:|---:|---:|
| 200,000 radios | 6.72 s | 4.82 s | 1.854 → 1.522 GiB |
| Repeated GPS | 1.56 s | 1.31 s | 1.682 → 1.001 GiB |

The high-cardinality import uses about **15% less peak RAM** and is about
**18% shorter**. Reopen peak RAM falls by **18–40%** on these fixtures. The
repeated-data import has a much smaller RAM improvement; it should not be
described as a uniform 15% saving. DuckDB reopens in about 0.07 s and has a
clear advantage on that metric.

On the high-cardinality fixture, the read-only observation adapter retrieves
the first radio's 50 complete rows at **10.56 ms p95**, versus **226.22 ms p95**
for the unindexed DuckDB candidate (200 samples each). SSID retrieval is limited
to 1,000 rows: **16.13 ms p95** for the adapter (200 samples), versus **42.61 ms**
for DuckDB's limited ten-sample diagnostic. The latter is not a 200-request tail
latency claim. DuckDB has faster catalogue SQL in this comparison. Three full
`SELECT count(*) FROM observations` adapter queries take about **0.415 s** each;
they return the real 9,999,900-row count without materializing another database.

The indexed DuckDB tuning pilot is a **single trial**: 31.03 s to import, plus
6.73 s to create radio and SSID ART indexes, 1.087 GiB peak process RAM, and a
1.203 GB database. Its measured radio query is 197.78 ms p95 (200 samples).
The unindexed database is about 0.352 GB, versus 0.915 GB for the complete
column-store workspace on the same high-cardinality input. These are the actual queries/plans
chosen for this workload; the result is not a general claim about ART lookup
performance. Index construction must be included when comparing a fresh
query-ready indexed database with the production store.

The existing GPU map was checked with a source-safe copy of the owned 10M-row
rendering fixture and **50,008 synthetic saved AP identities**. These saved
locations are rendering data, not newly computed fits. Across 200 requests:

| Map API | Warm p95 |
|---|---:|
| Clustered viewport, cached | 0.53 ms |
| Changing viewport / pan | 4.51 ms |
| First individual AP page | 2.51 ms |
| Survey counts, cached | 0.50 ms |
| Cached 500k sighting buffer | 1.51 ms |

The initial clustered request is 1.43 s, separately reported from warm results.
The full legacy compatibility fit response is 6.4 MB and 29.67 ms p95; the GPU
page uses the bounded feature endpoints instead.

A single 50M-row capacity trial accepts **49,999,500** rows and excludes 500
zero pairs. The column store imports them in **77.07 s / 38.93M rows/minute**,
with a **9.326 GiB** process peak. DuckDB imports them in **144.01 s / 20.83M
rows/minute**, peaking at **1.232 GiB**, and uses a 0.483 GB database. Both
finish within the 12 GiB guard. These are one-trial capacity observations, not
three-trial medians. The column store's RAM requirement at this size is substantial;
a machine with 8 GB RAM cannot be assumed to handle this workload comfortably.

The actual Wails/WebView2 GPU run retains 50,008 AP identities and their controls.
At 500k and 2M sighting settings, loaded pan-frame gaps are about **16.8 ms p95**.
The 8M setting contains about **5M actual visible sightings** in this fixture and
reaches **33.5 ms p95**. This dense case still misses a 60 Hz target. Frame gaps
are not GPU execution timers. Peak native engine working set is **2.56 GiB**;
engine plus WebView descendants peak at **3.384 GiB**. Dedicated GPU memory is
not included in those process working sets.

The matched 100-row incremental import initially took **3.804 s** in the column
store, because every index was rebuilt. The final append path takes a median
**77.5 ms across three independent 10M-row copies** (61.5–82.1 ms), about **49×
faster**. A fresh-process reopen verifies all **10,000,000** records after the
append. The unindexed DuckDB candidate takes **54.1 ms in one diagnostic trial**.
It still wins this small-write check, but the unnecessary full-survey rebuild is
removed. Changed posting lists can retain shared prior backing allocations until
a later full rebuild/reopen; observation columns are never duplicated by this
append path. Full rebuild remains available through administration.

The real scan-input check accepts **175,447** rows across the nine original
files and excludes **4,682** zero pairs. Source hashes are unchanged.

## SQL browser

Open **Administration → SQL browser**. The page provides schema/column browsing,
a query editor, Run, Explain, Cancel, previous/next pages, per-query timeout and
CSV export of the displayed page. The dialect is **SQLite**, including normal
SQLite REAL/decimal semantics. Integer and returned numeric values travel as
text to avoid JavaScript rounding; NULL stays distinct from an empty string.
Strings and schema names are rendered as text, not HTML.

`observations` is a private read-only virtual table over a captured immutable
State. Its SSID/radio equality searches reuse the production posting lists and
ordered IDs avoid an unnecessary SQL sort. Catalogue views resolve against the
same real observations. Metadata comes from a matching SQLite read transaction.
The empty compatibility observation/spatial tables in the metadata file are
not exposed as scan data, including explicitly qualified access.

Snapshot setup briefly takes the writer lock. Query execution releases it and
does not block map reads or copy the observation columns. One SQL query runs at
a time per application, and the HTTP survey lease protects its captured state.
Foreground SQL yields background preparation while running. Cancel, timeout,
page departure, survey handoff and app shutdown interrupt/join native work
before closing the survey. A cancelled query does not poison the normal pool.

Each page includes an opaque revision. A persistent read-only observer uses
[SQLite data_version](https://www.sqlite.org/pragma.html#pragma_data_version)
on the same connection, together with the observation State identity. Setup
retries if a metadata commit crosses snapshot capture. Paging refuses a changed
revision, including changes to comparisons/preferences or unavailable preparation
results; it does not silently mix successive database snapshots.

Limits: default 200 / maximum 1,000 rows; 256 columns; 20,000 cells; 2 MB of
returned cell text; long cells shortened around 4 KB. Default timeout is 30 s,
maximum 120 s, SQL text at most 100 KB. SQLite value/expression/opcode limits and
file-backed temporary sorting provide additional bounds. These are result and
execution limits, not a hard cap on total application RAM or SQL temporary disk.

The connection opens read-only; `query_only` and a deny-by-default authorizer
also reject mutations, DDL, transactions, ATTACH and PRAGMA changes. The native
parser accepts one statement followed only by comments/empty semicolons.
External file/extension helper functions are overridden and denied. The pinned
SQLite wrapper has an error-to-text behavior for custom scalar errors, so a
Go-side denial flag prevents those helpers from being treated as successful
queries. No write-enabled SQL mode is included.

## Evidence and reproduction

Raw commands, binary hashes, source hashes, sample timings, process peaks and
ceilings are retained under `build/benchmarks/phase4-uc_62_q7`. The committed
`benchmarks/baselines/phase4-summary.json` is the portable evidence record.
The `after-*` runs record an earlier index-only implementation and are excluded
from the final headline. `optimized-*` is a later intermediate build; `final-*`
contains the complete implementation, including incremental indexing.
The original Phase 0 inputs and the user's scan sources are never modified.

Build the isolated candidate as described in `tools/duckbench/README.md`, then
build the production probes with Go 1.27.3:

```powershell
go build -trimpath -o build/benchmarks/baseline-phase4.exe ./cmd/baseline
go build -trimpath -o build/benchmarks/baseline-phase4-final.exe ./cmd/baseline
go build -trimpath -o build/benchmarks/storagebench-phase4.exe ./cmd/storagebench
go build -trimpath -o build/benchmarks/storagebench-phase4-final.exe ./cmd/storagebench
go build -trimpath -o build/benchmarks/mapbench-phase4.exe ./cmd/mapbench
python benchmarks/phase4.py --engines before,final,duckdb
python benchmarks/phase4_checks.py --root <owned-run-directory> --capacity
python benchmarks/phase4_append.py --root <owned-run-directory>
python benchmarks/summarize_phase4.py --root <owned-run-directory>
```

The frozen “before” binary must be built from Phase 3 in a separate checkout,
not from the edited production sources. UI probes additionally require the
desktop/native benchmark builds and the independently cloned owned fixtures.
All probes enforce fresh import destinations and retain failed results.


## Verification

The full Go suite, Python/NumPy parity, vet, eight JavaScript map tests and four
benchmark-safety tests passed. After incremental indexing was added, all
observation-store, Atlas and engine tests were rerun, together with Python parity,
vet and race checks for appends, imports, immutable snapshots and SQL lifecycle.
The SQL adapter tests cover real rows/source fields, nullable values, large
integers, paging revisions, write/script rejection, cancellation/timeouts,
column/cell/byte bounds and non-fit metadata changes. Randomized append tests
compare thirty successive appends with full rebuilds and unchanged old snapshots.

Browser and real Wails/WebView2 SQL checks pass: schema, actual counts, paging,
Explain, write rejection, cancellation, safe markup, exact integer text and CSV
page export. The native export uses the production save-bytes bridge with a
fresh benchmark-owned destination replacing the interactive Save dialog. The
compact SQL page was visually inspected. The GPU native run verifies exact AP
counts, clustering/labels/rings/pins, preference round trips and sighting buffers.
Windows desktop builds pass. Other-platform packaging is not claimed.
