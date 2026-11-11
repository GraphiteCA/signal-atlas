# Phase 2: survey administration

The Admin page is reachable from Home, the GPU map and observation analysis. Phase 2 delivers plan items A01–A07 on the Phase 1 GPU-map base. The upstream baseline remains `6d8833659906faa89b1677424caf5f5cd905c56d`.

## User workflow

- **Survey & backups:** New database, Open database, ten recent workspaces, restore last survey on startup, complete workspace snapshots, portable SQLite export, saved SQLite conversion, and the app-wide fast-solver setting. Desktop actions use Wails file/folder dialogs; browser development mode has explicit path inputs.
- **Import scans:** native multi-file selection, dropped files and browser uploads, per-file outcomes, skip-invalid option, cancellation and paginated history. Readers cover WiGLE CSV, WiGLE Android backups, logger/WarHog tables, saved survey SQLite and Drive databases. Importing a portable survey contributes scans; conversion preserves its saved locations, map preferences and pinned comparisons in a new workspace.
- **Maintenance:** exact zero-pair preview, explicit cleanup confirmation, retained-revision restore, validation, map/index rebuilding, metadata WAL checkpoint and query-statistics refresh. Lookup rebuilding preserves saved fits. Maintenance runs in the background with progress/cancellation where safe.

New workspaces require a previously nonexistent directory. Open selects the **workspace folder**, not its internal `atlas.sqlite3`: that SQLite file holds metadata while observations live in column segments. Native snapshots include both. Portable SQLite exports materialize every observation, spatial indexes and all metadata for other tools. Workspace metadata files are rejected by the saved-survey importer to prevent a misleading empty survey.

## Import policy and accounting

Exactly two parsed numeric zero coordinates are excluded, including signed/scientific zero. A valid latitude of zero with a nonzero longitude, or vice versa, survives. Missing/non-numeric coordinates remain subject to measurement validation. The survey first separates non-Wi-Fi rows, then zero pairs, then other invalid measurements. The shared policy also applies to new imports into the independent Drive archive, without changing its support for BLE/cell/other records. Existing archive databases are not cleaned automatically.

`zero_excluded_rows` is independent of `invalid_rows`, `non_wifi_rows` and deduplication. A valid file containing only excluded zero pairs commits a zero-row history entry and hash, so importing it again is detected as a duplicate. Import history keeps the original `valid_rows`, current `active_rows`, and later `zero_removed_rows`. Cleanup does not rewrite filenames, timestamps, hashes or original accepted counts. Source files are read unchanged. Engine-owned upload/conversion scratch files are removed; native/original inputs are retained.

Imports commit per file. Cancelling a batch retains completed files and rolls back the in-progress import at its safe boundary. A batch displays individual file failures alongside successes. Hashing/index publication and some native filesystem operations finish their current step before cancellation takes effect.

## Session and persistence design

Survey API requests hold a session lease and carry `X-Survey-ID`; stale pages are rejected after a switch. AP cursors also include the session identity. The engine validates a candidate workspace before cancelling/joining old workers, drains old requests, swaps the Atlas/job runner and map caches, then closes the retired pool. One administration operation can run at a time. Conflicting survey writes are rejected while it runs; reads and app-owned progress/cancellation remain available except during the short handoff. Independent Compare sessions, exports, Drive jobs and app settings are not recreated.

Workspace and application-data file locks prevent concurrent writers. Recent/last-survey settings are written through a temporary file and replacement; fast-solver and update-consent values are preserved. Map display preferences have a versioned metadata payload, are survey-specific, and survive both snapshot formats. Camera views are keyed by workspace in local storage.

Cleanup copies metadata and the append-only dictionary into a new generation, reuses immutable segment files where possible, and rewrites only affected segment ranges. Surviving IDs and all measurement values remain intact. Named networks invalidate fits by SSID; hidden networks by MAC, including members of shared fits across scopes and solver namespaces. Unaffected result JSON and pinned comparisons survive. Immutable in-memory columns are shared while staging to avoid a second full copy; the old writer stays quiescent until handoff.

The publication point is a versioned `workspace.json` pointer with active/previous generation paths. Candidate metadata, segment manifest and validation finish before pointer replacement. Cancellation or an ordinary staging error removes only the newly owned candidate. A process interruption before publication leaves the old revision active; after publication the complete new revision is selected. Previous generations remain available for explicit recovery. Startup can select a previous complete generation when the published generation is missing or incomplete; it reports that recovery. Committed row counts are checked before orphan files can be swept, so a missing/partial manifest cannot silently discard import history or committed segments. An unavailable last workspace falls back to a usable default/recovery survey with a notice.

Native snapshots use hardlinks for immutable segments when the filesystem permits, otherwise copies. Metadata and dictionaries are independent copies. Subsequent imports/cleanup do not alter a snapshot. Portable SQLite export uses a sibling temporary file and replaces the destination only after a complete export; a failed/cancelled export preserves an existing backup. Retained revisions and interrupted unpublished candidates can consume disk space; this phase does not automatically prune them. Alternating zero/nonzero rows can create many surviving ID ranges; storage compaction remains later work.

## Measured checks

Windows/i7-10700K/32 GB host, eight scheduler slots, the owned Phase 0 high-cardinality fixture, uncontrolled filesystem cache. These are single-run checks, not percentile or cold-disk claims. No compilers/tests ran concurrently with the timed probes.

| Operation | Result |
|---|---:|
| Import 10M source rows | 18.00 s; 9,999,900 accepted, 100 zero pairs excluded |
| Accepted-row import throughput | 33.33M rows/minute |
| Import probe peak engine working set | 2.53 GiB |
| Preview legacy 10M-row survey | 15.2 ms; 100 rows in 2 networks |
| Stage, validate and publish cleanup | 5.64 s |
| Complete native workspace snapshot | 0.633 s |
| Open cleaned workspace after restart | 5.06 s |

The admin probe hashes surviving IDs and measurement fields before cleanup, after cleanup, after independently opening the snapshot, and after engine restart. All hashes match. Its whole-process peak is 4.54 GiB, including independent snapshot/restart checks that reload the survey; this is not the cleanup-only memory increment. Snapshot-open timing in the raw report includes its full fingerprint scan.

The nine original input CSVs produced **175,447 accepted observations**, with **4,682 zero pairs excluded**, and unchanged SHA-256 hashes. Their combined import time was approximately 1.30 s. Raw measurements, binary hashes, source hashes and smoke reports are recorded in [phase2-summary.json](../benchmarks/baselines/phase2-summary.json).

## Verification and reproduction

The Go suite includes Python/NumPy parity for unchanged accepted measurements and solver/query results, with explicit assertions for the newly added import-accounting fields. Phase 2 tests cover zero-only/duplicate inputs, CSV/WiGLE/saved-SQLite/archive filtering, surviving IDs and values, selective fit invalidation, pinned comparisons, session isolation, independent Compare/Drive preservation, writer conflicts, exclusive locks, native-dialog cancellation, scratch-file ownership, failed exports, staged cancellation, pointer recovery and incomplete manifests. Existing map/JavaScript and benchmark-safety checks remain in place.

The isolated Chrome smoke test exercises cleanup confirmation, browser file import, snapshots, New/recent switching and a 390px layout. Screenshots were visually checked. A separate hidden Wails/WebView2 probe checks admin data, preview and validation through the native host. Real OS dialogs are covered by host-interface cancellation tests; these probes do not automate user dialog choices. Production Windows desktop build and `go vet` are required checks. No SQL browser is exposed here; full-observation SQL remains Phase 4, and automatic location preparation remains Phase 3.

```powershell
go test -count=1 ./...
go vet ./...
node --test benchmarks/map-display.test.cjs
python -m unittest discover -s benchmarks -p test_run.py -v
go build -trimpath -o build/benchmarks/adminbench-phase2.exe ./cmd/adminbench
go build -trimpath -o build/benchmarks/baseline-phase2.exe ./cmd/baseline
go build -trimpath -tags desktop,production -o build/SignalAtlas-phase2.exe .
python benchmarks/phase2.py
python benchmarks/visual.py --data OWNED-UI-DATA --app build/SignalAtlas-phase2.exe --driver admin-smoke.cjs
```

`phase2.py` requires existing owned Phase 0 fixtures, creates a fresh output directory and clones their metadata/dictionary before opening them. It never opens the original fixture through the engine. The browser cleanup test requires a fresh legacy clone with zero pairs. All generated surveys, profiles and binaries stay under ignored `build/benchmarks`.
