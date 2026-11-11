# Large-survey map, administration and preparation changes

This is a coordinated follow-up to upstream `6d88336`. The existing Wails shell,
GPU map renderer, immutable observation store, reference/fast solvers, Compare
and independent Drive archive remain the foundation. The new work ports map and
administration behavior, prepares locations ahead of map navigation, and reduces
the cost of calculation and durable metadata publication.

## Review order

| Area | Main entry points | Behavior and evidence |
|---|---|---|
| GPU map controls and caches | `ui/map/`, `internal/atlas/mapcache.go`, `internal/engine/gpumap.go` | [Map behavior and measurements](phase1-map.md) |
| Survey administration and recovery | `internal/atlas/workspace.go`, `internal/engine/admin.go`, `internal/engine/survey_session.go`, `ui/admin.*` | [Administration contracts](phase2-admin.md) |
| Preparation lifecycle and events | `internal/atlas/preparation*.go`, `internal/engine/preparation.go`, `internal/engine/job_events.go` | [Resumable preparation](phase3-preparation.md), [latest metadata pipeline](metadata-preparation.md) |
| Observation memory and sampling | `internal/obs/`, `internal/atlas/sample.go`, `internal/atlas/fit.go` | [Memory/storage findings](memory-preparation.md) |
| SQL browser | `internal/atlas/sql_browser.go`, `internal/engine/sql_browser.go`, `ui/sql.*` | [SQLite adapter and limits](phase4-storage.md) |
| Optional GPU scoring | `internal/gpu/`, `internal/atlas/acceleration.go` | [Solver validation](phase5-fitting.md), [GPU throughput](viewport-preparation-gpu.md) |
| Hidden-network identity and quality research | `internal/atlas/selection.go`, `internal/atlas/hidden_identity_test.go` | [BSSID identity and quality recommendations](observation-quality.md) |

The phase commits remain separate for review. Publication replaces private paths
and input filenames in every phase, summarizes long timing/progress series, and
maps historical commit references to the equivalent published phase. Artifact
and original source hashes still identify the builds actually measured. The
original local history and full raw evidence were retained locally. No real scan
rows, survey files, logs, profiles or executable builds are part of this PR.

`ui/vendor/` contains pinned third-party map libraries and their licenses/hash
manifest. `benchmarks/baselines/` contains measured evidence, not application
source. These are marked vendored/generated in GitHub's diff to make the runtime
and test changes easier to review.

## What ships

- GPU map with exact AP clustering, clustering toggle, label zoom threshold and
  collision avoidance; solid planning rings with dark casing; radius 10 m by
  default, bounded to 5–50 m. Progressive individual delivery retains every
  eligible identity. Counts distinguish observations, names and located names.
- Bounded map/observation caches with revision and scope checks. Existing APs and
  observation buffers remain visible during live preparation and replacement
  requests; cancelled/late responses cannot replace newer views.
- Separate Admin screen for new/open/recent surveys, importing files/databases,
  consistent snapshots and SQLite export/conversion, validation, lookup rebuilds
  and maintenance. Native dialogs replace the need for command-line survey paths.
- Import-time exclusion of exact numeric 0,0 pairs, plus preview and recoverable
  cleanup for existing data. Equator/prime-meridian fixes with only one zero stay.
  Original input files, surviving observation IDs and unaffected fits are retained.
- Automatic whole-survey preparation, pause/resume, durable terminal outcomes,
  selective invalidation, bounded resource controls and post-commit live events.
  Visible sightings then a nearby halo get three of four dispatches while global
  work continues. Fits still use their complete selected observation scope.
- Smaller observation/dictionary allocations and bounded deterministic sampling.
  Atomic batched metadata writes overlap calculation. Per-target versions prevent
  stale saves; switching/maintenance/shutdown cancel and join all relevant work.
- Read-only SQLite SQL browser over the actual observation columns and a matching
  metadata snapshot, with schema inspection, Explain, bounded pages, cancellation,
  timeout and displayed-page CSV export. It is not a writable SQL console.
- Optional Windows/amd64 float64 OpenCL scoring with CPU validation and fallback,
  separate solver namespaces, immutable grid reuse, queued transfers and bounded
  scratch. Hidden selections consistently use BSSID/MAC rather than a blank label,
  including the older manual/shared selection path and its Python reference.

## Performance claims and their boundaries

All rows below identify their own workload and comparator. They must not be
multiplied into a single end-to-end speedup.

| Workload | Before → after | What it establishes |
|---|---|---|
| Fresh final-branch synthetic high-cardinality import, 10M source rows | 17.424 s; 34.43M accepted rows/min; 100 zero pairs excluded | One fresh import, 9,999,900 retained; 2.12 GiB peak across the import/query probe |
| Complete 5,753,026-scan / 1,460,561-target GPU-fast preparation | 336.004 s → 148.812 / 151.131 s | About 2.2× faster than the pre-metadata branch; identical complete saved results |
| Same full run, time inside batch saves | 215.754 s → 110.2–110.8 s | Reduced save cost; it overlaps calculation in the final pipeline |
| 50,008-AP complete response | Upstream 382 ms p95 → 30.2 ms in Phase 1 | Same identity count; separate clustered responses carry fewer symbols |
| GPU scoring, 10,201 candidates, 8 / 50 / 256 / 2,048 observations | 1.23× / 1.17× / 2.14× / 4.40× | Three paired scoring microbenchmarks, not whole-survey gains |
| Pan while 10,000 distant targets remain ahead of 64 local targets | 5.478 s → 0.194 s median | Synthetic scheduling/commit/map availability, three pairs |
| Phase 5 versus later memory work, same map-active 97,370-target cohort | 58.912 s / 4.40 GiB → 22.339 s / 2.89 GiB | Specific matched cohort; not the full-survey comparison above |

Final full runs produced 170,765 fitted outcomes, 1,289,796 unavailable outcomes,
338,826 saved estimate JSON rows and 1,460,561 checkpoints, with no pending work
or GPU fallbacks. Complete estimate, checkpoint-outcome, typed-location and
network-count hashes match. Final engine peaks were about 3.14 GiB, slightly
higher than the immediately preceding 3.09 GiB baseline because of writer caching
and overlap. Preparation throughput counts unavailable outcomes separately from
successful locations.

The release-path Wails GPU trace retained live preparation and fetching across
900 populated-view pan frames: 16.9 ms p95, no blank AP frames, no flashing loading
text, and 133.5 ms pause/join. It contained 61,233 sightings and 105 APs at sampling
time, so it is not a new hundred-thousand-marker render stress test.

Large private-survey files are not distributed. Timings use the documented
Windows/i7-10700K/32 GiB/RTX 2080 Ti host; other desktop activity/filesystem cache
were uncontrolled. Cross-platform core compilation is not a macOS/Linux desktop
performance measurement. Source/artifact hashes and failures/limited diagnostics
are retained in evidence; summarized traces are explicitly marked.

## Storage, migration and experimental boundaries

Production continues to use immutable Go observation columns plus SQLite
metadata. DuckDB is isolated in `tools/duckbench`, with no production dependency
or automatic conversion. Later tuning made its lower RAM attractive, but the
shipping provider is unchanged. Do not compare the older DuckDB full sweep to the
latest metadata pipeline as though both were retuned and measured together.

Metadata layout migration is transactional and preserves completed work. A full
completed legacy-copy first open took about 35 seconds once; subsequent reopening
took 6.47 seconds with zero GPU scoring calls. Existing files can reuse freed
pages; migration does not automatically vacuum/shrink them. It needs temporary
space for metadata copies. The dedicated recomputable-cache writer uses WAL
`NORMAL`; import/admin connections retain `FULL`. Power loss may require repeating
recent cache work. The checkpoint threshold is not a hard WAL-size cap.

Snapshots and cleanup retain recovery revisions and do not automatically prune
them. Workspaces are local files, and read-only SQL queries can still be expensive
within their limits. Reference fitting remains the default. GPU scoring is
optional and platform/device dependent; the display renderer and OpenCL solver
are separate capabilities.

Observation-quality rejection remains research, not a new import/default filter.
The tested GPS/ambiguity/consensus candidate improved known-location contaminated
fixtures but cost about 3.5× CPU time per tested fit. It has not demonstrated
accuracy against real AP ground truth. Shared nonempty names can still refer to
different physical APs; literal names such as `Hidden` cannot be globally treated
as exporter placeholders. The recommended next step is an opt-in quality pass
with exclusions/provenance and BSSID/site diagnostics.

## Validation and reproduction

Run `python scripts/check.py` for the portable local checks. It requires Go,
Python and Node, and refuses to silently skip their parity prerequisites.
Release building remains local; this PR does not publish a release or change
signing/update settings. Windows desktop packaging was tested; macOS/Linux
desktop packaging and signing need their native release environments.

The fast checks cover the full Go suite, recorded survey reference fixtures, Compare
worker parity, map-controller tests, benchmark safety/publication checks, vet
and pinned vendor hashes. `--race` adds lifecycle/cache/metadata/GPU-boundary race
tests. `--gpu` explicitly requires supported physical OpenCL hardware and executes
the score/solver integration checks. See [benchmarking](benchmarking.md) and the
phase documents for synthetic fixture generation. Recent portable entry points
are `benchmarks/metadata_perf.py` and `benchmarks/viewport_gpu.py`; their data
directories must have ownership markers. Historical diagnostic collectors refer
to owned run layouts and private-fixture hashes and are not turnkey public datasets.
