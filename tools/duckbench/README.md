# DuckDB storage candidate

This separate Go module is **benchmark only**. Production builds keep the selected
column store and pure Go SQLite driver. Nothing here opens the app's usual data
directory or runs a migration.

`candidate/store.go` and `candidate/import.go` are frozen copies of the donor
WifiLateration desktop app's DuckDB store/importer. They retain its vectorized
CSV parsing, normalization, spatial write order, persisted per-radio statistics,
transaction commit and file hashing. The benchmark adaptation excludes the
same `(latitude = 0 AND longitude = 0)` rows as the production importer and
reports those exclusions. Inputs must be supplied explicitly; database paths
must live under a benchmark ownership marker. Imports require a new database.

The pinned driver is `github.com/duckdb/duckdb-go/v2 v2.20000.0`. Building it
requires CGO and its platform native DuckDB library. These dependencies are
deliberately absent from the production module. Windows amd64 was measured;
other operating systems have not been benchmarked or packaged in this phase.

```powershell
$env:GOTOOLCHAIN='go1.27.3'
$env:CGO_ENABLED='1'
go build -trimpath -o ../../build/benchmarks/duckbench-phase4.exe .
go build -trimpath -o ../../build/benchmarks/duckbench-phase4-index.exe .
```

Run through `benchmarks/phase4.py` from the repository root. It supplies fresh
owned directories, the same input hashes, eight Go/native worker slots and
process memory monitoring. DuckDB's 1 GB setting limits its buffer manager;
the evidence separately records total process peak working set. It is not a
hard process memory cap. Filesystem cache is uncontrolled.

The comparison includes complete import commits, disk size, independent
process reopen, observation viewport reads, SSID/radio SQL retrieval and
catalogue reads. It does not pretend that this isolated importer already
implements the production app's preparation checkpoints, selective invalidation,
recoverable workspace generations, GPU caches or full cross-reader parity.
The storage decision documents those limits. No identity remapping, deduplication
change or conversion is applied to an existing survey.

`preparation/` adds a complete-sweep observation adapter. It imports the parent
module's real grouping, sampling, fitting and batch checkpoint writer through a
local module replacement. Build with:

```powershell
go build -trimpath -o ../../build/benchmarks/duckprep.exe ./preparation
```

The preparation comparison first converts canonical typed native rows, including
their original IDs, through the DuckDB appender. It then reads 256 targets at a
time and fits bounded read-only views with the production Go solver. It uses an
independent SQLite metadata copy for the same durable checkpoint semantics.
This measures observation storage and batched reads; it does not replace the
production metadata engine. See [preparation performance](../../docs/preparation-performance.md)
and `benchmarks/preparation_perf.py` for full-sweep commands and output checks.

Primary documentation:

- [Go connection and configuration](https://duckdb.org/docs/current/clients/go/connecting)
- [Memory settings and limitations](https://duckdb.org/docs/current/guides/performance/oom)
- [Buffer manager limit](https://duckdb.org/docs/current/configuration/pragmas)
