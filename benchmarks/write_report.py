"""Write the phase-0 review from aggregate evidence, without private records."""
import json
import statistics
from pathlib import Path
import run as bm

s=json.loads((bm.ROOT/'benchmarks/baselines/phase0-summary.json').read_text())
if s['phase0_status']!='complete':raise SystemExit('baseline evidence is incomplete')
by={(r['engine'],r['fixture']):r for r in s['imports']}
native={r['renderer']:r for r in s['native']}
sp=s['special']
lines=[
'# Phase 0 baseline results',
'',
'Phase 0 is complete on upstream [`6d88336`](https://github.com/olosnoks/signal-atlas/commit/6d8833659906faa89b1677424caf5f5cd905c56d), verified as current GitHub main during this run. The application code is unchanged. Baseline completion does not mean every performance target passed.',
'',
'**Recommendation:** use the upstream GPU map as the foundation for the map port, add exact backend clustering and cached map responses, and retain the current Go backend for that first feature port. Keep the storage decision separate: DuckDB uses much less memory and reopens faster, while the current backend imports and serves observation overviews faster in these tests.',
'',
'One full donor high-cardinality trace was interrupted after about thirteen minutes and replaced with limited diagnostic samples. The exception and two invalid early native-driver runs are preserved in the aggregate JSON; none is presented as a completed 200-request trace.',
'',
'## Host and scope',
'',
'Windows; Intel i7-10700K (8 cores/16 logical CPUs); 32 GB RAM; RTX 2080 Ti; Intel RAID 0 volume. Controlled engine runs use GOMAXPROCS=8 and eight configured workers. The donor uses DuckDB v2.0.0 with its existing **2GB working-memory setting**, which is not a process-memory cap. Native Wails/WebView2 runs use the app’s normal scheduler profile and a 1400×950 window.',
'',
'Filesystem cache was uncontrolled. Reopen means a new process, not cold physical storage. These are synthetic repeated-position and high-cardinality fixtures plus a separate correctness/performance run on the nine local input files. Native AP stress locations are synthetic rendering fixtures, not solver-quality results.',
'',
'## Imports and capacity',
'',
'Rates are millions of newly accepted Wi-Fi rows per minute. The 10M cases have three independent import trials; 50M has one. Memory is the largest process working-set peak across the complete import/query/fitting probes, not only import memory.',
'',
'| Fixture | Trials | Upstream median M rows/min | Donor median M rows/min | Upstream peak GiB | Donor peak GiB |',
'|---|---:|---:|---:|---:|---:|',
]
for stem,label,n in [('repeated-10m','10M / 1,000 radios / repeated GPS',3),('cardinality-10m','10M / 200,000 radios',3),('capacity-50m','50M / 1,000 radios / repeated GPS',1)]:
    u=by['upstream',stem];d=by['donor-duckdb',stem]
    lines.append(f"| {label} | {n} | {u['import_million_rows_per_minute']['median']:.2f} | {d['import_million_rows_per_minute']['median']:.2f} | {u['peak_engine_working_set_gib']:.2f} | {d['peak_engine_working_set_gib']:.2f} |")
lines += [
'',
'Both engines exceeded the 10M rows/minute target on these bulk fixtures. Upstream’s repeated-position trials ranged from 46.00–49.25M/min; the high-cardinality trials ranged from 31.60–32.40M/min. The normal 16-slot scheduler import profile also completed 10M rows in 12.02 s (49.92M/min); it is a single supplemental trial.',
'',
'The upstream 50M import took 61.82 s and stayed just below the 12 GiB engine ceiling at 11.79 GiB. The donor took 156.01 s and peaked at 2.20 GiB. This is a substantial RAM tradeoff, not a universal storage winner. The 50M fixture is highly repetitive; do not assume the same compression or memory use for every survey.',
'',
'### Fresh process open and first summary',
'',
'These numbers include opening the store and executing the first whole-survey summary. Upstream uses the median of its completed trials; the donor readiness check is one fresh-process diagnostic per fixture. They do not represent every possible first query.',
'',
'| Fixture | Upstream open + summary, s | Donor open + summary, s |',
'|---|---:|---:|',
]
for stem in ['repeated-10m','cardinality-10m','capacity-50m']:
    u=by['upstream',stem]
    ready=statistics.median(a+b for a,b in zip(u['process_cold_reopen_seconds'],u['process_cold_first_summary_seconds']))
    d=s['fresh_process_donor_open_and_first_query']['cold-donor-'+stem]['open_plus_first_summary_seconds']
    lines.append(f'| {stem} | {ready:.3f} | {d:.3f} |')
lines += [
'',
'DuckDB loads data on demand; the column store loads observations and builds indexes at open. Survey switching must account for the cost of keeping an old large store alive while a candidate loads.',
'',
'## Map response and native rendering',
'',
'The broad observation-map query missed the provisional 250 ms p95 target. The representative 200-request upstream traces reached about 320 ms p95 on repeated-position data and 734 ms on high-cardinality data. The donor repeated-position trace reached about 658 ms p95. Its limited high-cardinality diagnostic had a 1,877 ms median across five requests; that is not a 200-request p95.',
'',
'For approximately 50,000 saved APs, the upstream uncapped GPU API measured **382 ms p95**; the legacy sampled API measured **242 ms p95** over 200 requests each. The legacy API returns only a subset, so the amounts of data differ.',
'',
'The following native camera traces reuse loaded data, with only the application’s auto-fetch entrypoint disabled by the diagnostic driver. Map and renderer event listeners remain active. Separate API traces measure data fetching. RAF gaps measure frame pacing, not GPU timer-query execution.',
'',
'| Native path | Shown APs | Shown observations | p95 frame gap, ms |',
'|---|---:|---:|---:|',
]
for n in s['native']:
    for case in n['scenarios']:
        name=n['renderer']
        if name=='upstream MapLibre/deck.gl':name='GPU / '+case['id']
        elif name=='donor Leaflet single canvas':name='Donor canvas / clusters '+('on' if case['clustering'] else 'off')
        aps=case.get('saved_aps',case.get('shown_saved_aps',0));obs=case.get('shown_observations',0)
        lines.append(f"| {name} | {aps:,} | {obs:,} | {case['pan_frame_p95_ms']:.1f} |")
gpu=native['upstream MapLibre/deck.gl'];legacy=native['upstream Leaflet explorer'];canvas=native['donor Leaflet single canvas']
lines += [
'',
f"The GPU AP-only load/display step took {gpu['scenarios'][0]['first_usable_ms']:.0f} ms for 50,008 locations; the legacy explorer took {legacy['scenarios'][0]['first_usable_ms']:.0f} ms while showing 1,471 sampled APs. GPU rendering was confirmed on ANGLE/Direct3D11 using the RTX 2080 Ti. The 8M observation limit actually selected about 5M points because of the current every-kth thinning algorithm; it did not draw 8M points.",
'',
f"Whole native-run peak working sets were {gpu['peak_family_working_set_gib']:.2f} GiB for the GPU run, {legacy['peak_family_working_set_gib']:.2f} GiB for the explorer and {canvas['peak_family_working_set_gib']:.2f} GiB for the donor component. GPU memory includes the large observation scenarios. Dedicated GPU VRAM was not measured.",
'',
'The donor canvas is a component comparison in the same Wails host using the same AP payload and bundled Leaflet. It includes clustering and planning rings; the current GPU page has fewer AP presentation features. The result supports porting those behaviors into the GPU path, with a bounded canvas fallback, rather than copying either UI wholesale.',
'',
'## Correctness, imports and administration',
'',
'- The Python reference and upstream import agreed on **180,129 Wi-Fi rows** across nine real input files. Importing them took 1.40 s. Of those accepted rows, **4,682 are at 0,0**; the new exclusion policy would retain 175,447. No live survey or original input was altered.',
'- The 1M-row Drive archive imported in 4.56 s, exported CSV in 1.16 s and recognized all 1M rows as duplicates on export/reimport. Whole-file duplicate detection and per-day export also passed.',
f"- Twenty-five small imports preserved 31 of 32 saved fits and invalidated the changed target. Median/p95 per-import times were {sp['incremental']['small_import_p50_ms']:.1f}/{sp['incremental']['small_import_p95_ms']:.1f} ms on the 1M-row store. Batching small imports is worth evaluating because index-publication overhead dominates tiny files.",
f"- Active cancellation stopped workers in {sp['incremental']['cancel_join_ms']:.1f} ms in this test and resumed work reused committed outcomes. The existing progress counter advanced to 256 completed targets while only two outcomes had been processed. Preparation checkpoints must use actual per-target outcomes, not that batch progress counter.",
f"- A complete indexed 10M-row SQLite snapshot took {sp['export-10m']['export_seconds']:.1f} s to export and {sp['export-10m']['snapshot_import_seconds']:.1f} s to load into a new store. The reopened count matched 10M. This check used a quiescent survey; concurrent snapshot consistency remains an administration-phase test.",
'- The real-input 64-group subset produced 51 fitted outcomes in roughly 0.18 s in either full-data solver mode. Thirteen outcomes were unavailable. This subset does not establish a general solver-throughput or accuracy guarantee; no additional solver port is justified by this result alone.',
'',
'Validation passed: 15 upstream Go tests including Python/NumPy and Node parity; 49 donor Go tests; 62 Python reference tests; four Python harness safety checks and one Go ownership check. Go vet passed for the new commands. The donor’s subprocess-helper entry is deliberately skipped in the outer test run; its parent file-lock test passed. There were no missing parity prerequisites.',
'',
'## Performance work to carry into the next phases',
'',
'1. **Phase 1: integrate the GPU map and backend clustering.** Keep the clustering toggle, label threshold, solid rings, compact totals and 10 m/50 m radius behavior. Bound returned features and fetch details on demand. The dense AP fetch still misses the 250 ms target even though loaded GPU panning is smooth.',
'2. **Cache map data by scope and revision.** The observation diagnostic attributed about 73% of sampled CPU time to the aggregation scan, with substantial distinct-ID hash-map insertion. The dense AP diagnostic spent about 43% of total sampled CPU time in SQLite execution. Evaluate typed spatial caches/materialized display fields and incremental invalidation; retain exact counts.',
'3. **Keep storage evaluation before committing to the long-term database format.** The current backend wins bulk import and these observation queries; DuckDB wins working-set size and fresh-process readiness. Benchmark additional DuckDB memory budgets and populated AP query fixtures before a backend replacement. Avoid doubling large-store RAM during survey switches.',
'4. **Phase 2: prioritize 0,0 hygiene and honest save progress.** Implement exclusion and recoverable cleanup with corrected source counts. Consider a consistent native workspace bundle for fast snapshots while retaining the slower portable SQLite export.',
'5. **Phase 3: use committed target records for resume/progress.** Preserve untouched fits, handle the cancellation counter issue and measure real UI delivery. A ≤500 ms committed-update guarantee is not established by the current polling-based UI; worker cancellation and display latency are separate measurements.',
'',
'The 10M bulk-ingest target passed. The dense viewport target did not. Native loaded-data frame pacing is established; a separate ≤16 ms GPU execution-time guarantee was not measured. The next implementation work is the map integration and data-delivery path, with the storage tradeoff retained explicitly.',
'',
'## Evidence and reproduction',
'',
'- [Aggregate JSON and source manifests](../benchmarks/baselines/phase0-summary.json)',
'- [Measurement methodology and commands](benchmarking.md)',
'- [Workload manifest](../benchmarks/manifests/windows-baseline.json)',
'- Observation CPU/heap profile: `build/benchmarks/extra-d37i8w30/profile/`.',
'- Dense AP CPU/heap profile: `build/benchmarks/extra-i28xdj0w/dense-profile/`.',
'- Raw trial directories and excluded-run reasons are recorded in the aggregate JSON. Source freezes, private real-input results, native diagnostics and test logs remain under ignored `build/benchmarks/`.',
'',
'Do not reset the baseline by overwriting these fixtures or normal survey files. Future feature phases should rerun the same hashes, hardware profile and correctness assertions, recording accepted-row and solver-policy changes separately.',
]
path=bm.ROOT/'docs'/'phase0-results.md';path.parent.mkdir(exist_ok=True)
path.write_text('\n'.join(lines)+'\n',encoding='utf-8')
print(path)
