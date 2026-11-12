# Viewport-first preparation and GPU throughput

> Historical implementation and measurement report. Claims about defaults,
> validation and upstream revisions refer to the work recorded here. For current
> behavior and checks, use the [documentation index](README.md).

Base revision: `80fcc3d`. Measurements: 11 November 2026, i7-10700K,
32 GiB RAM, RTX 2080 Ti, driver 621.01, Go 1.27.3.
Machine-readable evidence: [viewport-preparation-gpu.json](../benchmarks/baselines/viewport-preparation-gpu.json).

## Delivered behavior

Panning or zooming the GPU map now prioritizes unfinished networks with sightings
inside the visible area, followed by a surrounding halo. The halo extends each
axis by a quarter of the viewport, with a minimum of 500 metres. When both queues
have work, three of every four dispatches take local work and the fourth takes
global work. Running calculations finish; camera movements replace unclaimed
queue entries without restarting fits or invalidating completed locations.

Priority uses the existing immutable spatial postings, bounded 16,384-row scans,
512-target refills and a capped deduplication set. Visible sightings precede halo
sightings even when both occupy the same spatial cell. Very broad views spanning
over 4,096 cells use ordinary global preparation. Repeated refreshes of an unchanged
camera do not restart the scan. Per-page sequence numbers reject late pan hints;
these hints never enter solver, map-cache or observation-buffer keys.

This is a scheduling change. A prioritized network still uses its complete
observation history under the selected preparation settings. Before calculation,
the app can identify nearby sightings, not an unknown future fitted position.
Networks without sufficient usable observations still cannot produce a location.

The preparation status shows “current view first” while local work is queued.
The bottom-left “Individual AP / 12 nearby APs” was a static legend example,
not a measured count. It has been removed. The actual AP count remains in the
map status, and a selected cluster is described as “APs in this cluster”.

## GPU changes

The OpenCL path previously blocked the host on each of eight input uploads.
It now queues nonblocking writes, keeps the Go backing arrays GC-pinned until
the in-order queue completes, and fences before releasing storage on errors.
A failed driver fence disables that queue and retains the single bounded pin
lease rather than risking outstanding access to released memory. CPU fallback
remains available.

The standard immutable coarse grid is validated once and reused on the device
between compatible calls. Refinement and other grids invalidate that reuse.
Tiles permit up to four million candidate/observation products rather than one
million, still within the existing 64 MiB scratch limit. This reduces dispatch
overhead and exposes more parallel work for long histories. Float64 arithmetic,
candidate order, CPU validation, fit semantics and cache namespaces are unchanged.

The transfer lifetime follows the [OpenCL nonblocking-write contract](https://registry.khronos.org/OpenCL/specs/unified/refpages/man/html/clEnqueueWriteBuffer.html).
GC pinning is not a claim of page-locked host allocations or simultaneous
kernel execution. The shared display GPU still needs time to render the map.

## Measurements

Three paired trials alternated base/current execution order. Real OpenCL calls
include uploads, kernels, blocking result reads, validation and hashing every
score. Each call scores 10,201 candidates. Small shapes use 300 calls per trial;
the 2,048-observation shape uses 60. These are scoring microbenchmarks, not
whole-survey speedups or GPU-utilization targets.

| Observations per calculation | Base calls/s, median | Current calls/s, median | Gain |
|---|---:|---:|---:|
| 8 | 1,775.69 | 2,190.71 | 1.23× |
| 50 | 1,299.86 | 1,520.64 | 1.17× |
| 256 | 263.22 | 563.31 | 2.14× |
| 2,048 | 4.68 | 20.63 | 4.40× |

Every score bit matches the base revision in every paired trial. The explicit
hardware suite also checks weighted/multiple-group scores against the CPU and
identical final fast/reference coordinates and SSE.

The viewport fixture contains 10,000 distant and 64 local targets, with eight
scans each. It starts real eight-worker GPU-fast preparation, waits for at least
100 global saves, then pans to the local area. All 64 local results were saved
and returned by the actual map endpoint. Across three pairs, median time from
the pan to all local saves changed from **5.478 seconds to 0.194 seconds**,
about **28× sooner**. Current runs completed only 415–476 additional targets
before local results were ready; the baseline had to finish the remaining
9,728–9,804 targets. This synthetic queue test demonstrates scheduling latency,
not a promise for every town or dataset. Commits still use bounded batches.

An independent copy of the real 5,753,026-row survey, with the existing
97,370-target cohort, completed in **22.858 seconds before / 21.740 seconds
after**, a roughly **5%** elapsed improvement in one trial each. Both produced
11,299 fitted and 86,071 unavailable targets and the same 22,584 saved estimate
rows, with identical complete estimate hash:

`0acf515409e7747b9263da653cc47ef01a8101ac0e38909a306c5dfef303af2f`

Peak engine working set was 2.638 / 2.642 GiB. This change does not replace the
resident observation store or materially lower its RAM. The complete sweep
still includes catalog checks, targets needing no solver, and serialized metadata
commits; higher GPU activity cannot accelerate all those stages. No new full
1.46-million-target sweep or DuckDB comparison was run for this change. The
previous [memory/storage findings](memory-preparation.md) remain applicable.

Other desktop activity and the filesystem cache were uncontrolled. Benchmarks
ran sequentially without competing agent tests. No private records or original
survey files were changed; all fitting runs used independent owned copies.

## Desktop and correctness checks

The real Wails/WebView2 GPU page completed 900 pan frames during actual preparation:
16.8 ms p95 frame time, 6.5 ms pause/join, no blank AP frames, no flashing loading
frames, no JavaScript errors, and no GPU fallbacks. It dispatched 13 targets through
the spatial priority path. The initial view loaded 479,407 sightings; the pan trace
anchored one actual AP at zoom 15 with 317 local sightings. This is not a new dense
marker or maximum-history GPU render stress test. Peak simultaneous process-family
working set was 3.406 GiB.

Validation passed: full Go suite with active Python numerical parity, vet,
targeted preparation/map/viewport/GPU race checks, physical GPU score/solver
checks, 13 JavaScript controller tests, seven fixture safety tests, and Linux
amd64/macOS arm64 core builds with CPU fallback. Cursor tests cover hidden
networks, visible-before-halo ordering with one-item pages, cancellation, stale
inputs, completed targets, complete fit inputs, rapid pans and global fairness.

## Reproduction

`cmd/gpubench` measures real scoring and hashes all outputs.
`cmd/viewportbench -build -data <owned-new-data>` creates the synthetic fixture;
without `-build` it measures actual saves and map availability after a pan.
Both commands reject unsafe shapes or unowned preparation fixtures.

Freeze the base revision in an ignored benchmark directory with an `owner.json`
containing `{"purpose":"signal-atlas-benchmark","schema":1}`. Copy the benchmark
commands into that source snapshot. For the older GPU API, remove the
`-fixed-grid` flag and its `gpu.NewCandidateGrid` block from the baseline's
`cmd/gpubench/main.go`; all observation generation, candidate coordinates,
mutations, hashing and measured loop operations remain identical. Build the
before/after commands into the owned root as `gpu-before.exe`, `gpu-final.exe`,
`viewport-before.exe` and `viewport-after.exe`, then run:

```text
python benchmarks/viewport_gpu.py --root <owned-root>
```

The runner alternates paired trials, clones independent fixture metadata, checks
all score hashes and all 64 map results, samples process memory and writes raw
evidence under that root. `cmd/fullprepbench` and `benchmarks/memory-native.js`
provide the separate real-data sweep and desktop checks. Do not open original
surveys with preparation enabled merely to reproduce a benchmark.

Packaged desktop build: `build/SignalAtlas-viewport.exe`, using
`go build -tags desktop,production -ldflags "-H windowsgui -X main.Version=viewport-gpu"`.
The running app was not replaced or stopped.
