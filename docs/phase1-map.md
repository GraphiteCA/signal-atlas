# Phase 1: GPU survey map

> Historical implementation and measurement report. Claims about defaults,
> validation and upstream revisions refer to the work recorded here. For current
> behavior and checks, use the [documentation index](README.md).

The home page now opens the GPU survey map as the primary map. Observation analysis has been retired from the app, including its routes, embedded assets and map handoff. Admin and Compare remain available from map navigation. The obsolete Python implementation has also been removed; Go regression tests use recorded reference fixtures.

## Delivered behavior

- Exact, stable world-grid AP clusters; click a cluster for its real members and a zoom action. Badges use cell centres to keep their exact counts readable. Bounds and individual AP coordinates retain their original positions.
- Clustering toggle; labels from zoom 2–22, default 18, with **Any zoom** at 2. Collision placement uses a bounded screen occupancy grid; densely coincident symbols cannot cause quadratic rectangle scans. Labels avoid other symbols and labels.
- Solid coloured planning rings with dark casings. Radius defaults to **10 m**, allows **5–50 m**, and is explicitly a planning radius rather than a confidence interval.
- Compact, whole-survey counts: **Located SSIDs**, **Observed SSIDs**, **Stored observations**. These do not change when a map filter, selection or viewport changes. Located SSIDs are distinct nonempty names still present in observations with a saved fitted numeric location in any scope/solver; hidden names, unavailable outcomes and missing coordinates do not inflate the count.
- Individual AP details, independent pins, cluster/coincident-member browsing, keyboard-accessible member buttons, and Escape to close details. Pinned APs are removed from overview counts and drawn separately at their actual coordinates.
- Clustering off progressively loads **every eligible AP**, with “N of M” progress, cancellation and revision-bound cursors. Page limits are delivery budgets, not a total AP limit. Member lists load 20 at a time with an explicit Load more action.
- Shared AP, individual radio and combined models; band, SSID, signal and date filters; existing Fit every AP/cancel operations. Full-input fits replace sampled fits of the same identity, with fast/reference solver namespaces kept separate.
- Clustering, label threshold, ring visibility and radius are stored in the survey's metadata, survive export/reopen, and never change fit revisions or trigger fitting.
- Pinned MapLibre 4.7.1, deck.gl 9.4.1 and Leaflet 1.9.4 assets, licences and hashes are bundled under `ui/vendor`. Tiles retain the existing bounded server cache. The GPU map initializes from style readiness without waiting for online raster tiles. A single-canvas Leaflet fallback uses the same exact AP API and controls, including normalized 256/512-pixel world scales.

## Loading and cache design

`internal/atlas/mapcache.go` retains a typed, longitude-indexed location snapshot for the active solver/scope. It decodes saved JSON once, applies current group eligibility, and uses binary search for longitude bounds. Group eligibility aggregation is reused across viewport requests and saved-fit changes; only changed observation state or filters/model require it again. World-cell keys are reused at the current integer zoom.

SQL triggers advance a metadata revision on estimate insert/update/delete. Rollbacks do not advance the committed revision. Immutable observation State identity detects imports, database replacement and band edits, including cases with no deleted estimates. Both identities participate in invalidation. A stale cursor returns HTTP 409 and the client restarts the view. Cached survey totals use the same revision mechanism.

Retention is limited to one active fit scope, target set and viewport, with an estimated 128 MiB fit-index retention budget and 16 MiB viewport budget. Oversized indexes still serve every AP but are not retained. These are cache budgets, not caps on the underlying column store or total process memory. AP pages allow at most 1,500 features and target 900 KB; a single unusually large imported identity can exceed the byte target. The measured dense individual page is about 284 KB.

When a large individual view would exceed the full-response retention budget, pagination caches compact row indices (up to 16 MiB) and constructs only the requested page. It does not rebuild or sort the entire feature payload for every page. A 110,000-AP pagination test verifies complete delivery without retaining that oversized payload.

Packed observation responses retain one view up to 64 MiB, keyed by all request parameters and observation state. Cancelled point scans join their workers before returning. Larger buffers are served without retention. The existing every-kth sighting thinning policy is unchanged.

The client cancels superseded fetches, rejects stale responses, loads APs independently of large sighting buffers, and limits progressive GPU uploads to ten per second. Immutable observation/ring data stays on the GPU between camera moves. Unchanged layers are not resubmitted for a second draw on each pan; the map overlay's existing camera handlers remain active.

## Measured results

Same Phase 0 Windows host and owned fixture: **10M observations, 50,008 saved APs**, synthetic rendering locations; i7-10700K, RTX 2080 Ti, 32 GB RAM. Engine API traces use eight scheduler slots and 200 requests. Filesystem cache is uncontrolled. Cold cache-building requests are separated from warm traces; this is not cold-disk or whole-app startup timing.

| API trace | Phase 1 p95 |
|---|---:|
| Exact clusters, repeated view | 1.0 ms |
| Exact clusters, changing viewport | 4.5 ms |
| Individual AP page | 2.0 ms |
| Full 50,008-AP compatibility response | 30.2 ms |
| Repeated packed 500k-sighting buffer | 2.5 ms |

The full AP response was **382 ms p95 in Phase 0**, so caching/serialization improve that path about **13×** while returning the same number of AP identities, now also with IDs/bands. The clustered response represents the same 50,008 identities with 13 symbols and approximately 2 KB; it is not the same payload as the full individual response. The first AP cache build takes **1.58 s** after opening the store; the first global count calculation takes **396 ms**. Warm count requests are below the timer's useful resolution. Cold opening of the in-memory observation store remains a separate Phase 0 cost.

| Native GPU scenario | AP identities | Rendered AP symbols | Sightings | p95 RAF gap |
|---|---:|---:|---:|---:|
| Clusters, rings enabled | 50,008 | 13 | 0 | 16.8 ms |
| Clustering off, rings enabled | 50,008 | 50,008 | 0 | 16.9 ms |
| Clustering off, Any zoom labels, rings | 50,008 | 50,008 | 0 | 16.8 ms |
| Clusters + 500k point limit | 50,008 | 13 | 499,983 | 16.9 ms |
| Clusters + 2M point limit | 50,008 | 13 | 1,999,934 | 17.1 ms |
| Clusters + 8M point limit | 50,008 | 13 | 4,999,850 | 33.6 ms |

All individual AP pages finished loading in **871 ms** in the native GPU test; enabling collision-managed labels at Any zoom finished in **1.52 s**. The cached clustered load/display step took **30 ms**. These are warm post-startup scenarios, not launch times. Collision rejection intentionally leaves only eight readable labels in the dense 50k-AP scenario while every AP symbol remains available.

The fallback's clustered case stayed near 60 fps. Drawing all 50,008 rings in the canvas fallback is slower (about 133 ms p95 frame gaps); clustering remains its default. Native tests confirmed WebGL 2 / ANGLE / RTX 2080 Ti. RAF gaps measure frame pacing, not GPU execution time or VRAM. Camera traces reuse loaded buffers with only app auto-fetch disabled; renderer/camera listeners remain active. The largest sighting case retains the upstream thinning behavior and does not claim to render eight million points.

Raw paths, memory peaks, final renderer reports, exclusions and API evidence are in [phase1-summary.json](../benchmarks/baselines/phase1-summary.json). Earlier development probes that selected fallback through an incorrect capability helper or preceded buffer/label optimizations are excluded from final performance claims.

## Verification and reproduction

The full Go regression suite (including Python/NumPy parity) passed. New map tests cover exact counts beyond 1,500 APs, pagination, pins, stale cursors, import/band/SQL invalidation, rollback, solver isolation, full-input precedence, preference round trips and API guards. Seven JavaScript tests cover controls, collisions, stale requests, progressive pages and stable GPU data objects. Benchmark safety tests and Go vet passed.

Native probes cover GPU and canvas rendering, cluster member details, pins and preference writes without fit-revision changes. The isolated browser smoke test verifies AP details and both renderers with CDN/raster delivery blocked; its screenshots were visually reviewed for badge spacing, solid rings, detail text and fallback controls. Test data and profiles remain under ignored `build/benchmarks`; no live survey was cleared or imported.

```powershell
go test -count=1 ./...
node --test benchmarks/map-display.test.cjs
python -m unittest discover -s benchmarks -p test_run.py -v
go build -trimpath -o build/benchmarks/mapbench-phase1.exe ./cmd/mapbench
go build -trimpath -tags desktop,production -o build/benchmarks/nativebench-phase1.exe ./cmd/nativebench
go build -trimpath -tags desktop,production -o build/SignalAtlas-phase1.exe .
build/benchmarks/mapbench-phase1.exe -data build/benchmarks/run-EXAMPLE/trial-1/store -requests 200
build/benchmarks/nativebench-phase1.exe -repo . -data build/benchmarks/run-EXAMPLE/trial-1/store -page gpu -driver phase1-native.js -output NEW-OUTPUT.json
python benchmarks/visual.py --data build/benchmarks/run-EXAMPLE/trial-1/store
```

Use the same prepared Phase 0 render fixture with 50,008 APs. The browser smoke assertions deliberately require that fixture. `mapbench` and `visual.py` require an ownership marker; use a new output path/profile for each run. For native fallback testing, pass `-page canvas`. Phase 0 tools/default diagnostic script remain available. Bulk import and solver-speed benchmarks were not repeated for this map phase; their Phase 0 evidence remains unchanged.
