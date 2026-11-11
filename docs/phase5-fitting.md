# Phase 5: solver comparison and optional GPU scoring

Base: `8af4acc`, after the complete native/DuckDB preparation comparison.
Branch: `codex/fit-performance`. The production store remains immutable columns
and SQLite metadata. Reference fitting remains the default; GPU acceleration and
the existing approximate fast solver are independent opt-in settings.

## Delivered behavior

In **Admin → Survey & backups → Calculation preference**, enable **Use GPU
acceleration (experimental)**. The page reports the selected physical GPU or a
CPU fallback reason. This applies to background preparation and manual fitting,
and to the matching saved-location lookups. Leave **Use the fast location
solver** unchecked to accelerate the reference algorithm.

The optional backend supports Windows amd64 GPUs with a working OpenCL 1.2
runtime and float64 support. The tested device is an NVIDIA GeForce RTX 2080 Ti,
driver 621.01. It loads the installed system OpenCL DLL on demand; no Python,
CUDA toolkit, CGO dependency or additional runtime is bundled into the app.
Unsupported platforms, unavailable devices, oversized inputs and detected
driver/numerical errors use CPU scoring. CPU operation does not initialize the
OpenCL driver. Linux amd64 and macOS arm64 engine builds pass with CPU fallback;
native runtime testing in this phase is Windows only.

Large candidate grids are scored on the GPU in float64, with contraction and
fast-math optimizations disabled. Grouping, deterministic sampling, observation
weights, search grids, separated starts, model parameters, final coordinates,
tie checks and checkpoint semantics retain the existing method. Reference
refinement grids can also be scored on the GPU. Fast-mode one-point refinement
stays on the CPU to avoid transfer overhead.

Competitive coarse cells and nearly tied refined minima are rescored using the
original CPU arithmetic. A discrepancy beyond the validation tolerance disables
the GPU for the process and falls back. This is a conservative check rather
than a proof of bitwise equivalence on every GPU; the actual workloads below
require identical final numeric results. OpenCL defines feature/precision
requirements independently of a particular CPU implementation. See the
[OpenCL C specification](https://registry.khronos.org/OpenCL/specs/unified/html/OpenCL_C.html)
and [OpenCL API specification](https://registry.khronos.org/OpenCL/specs/unified/html/OpenCL_API.html).

One process-wide queue serializes kernel-argument/buffer changes. Inputs and
outputs transfer synchronously with Go memory pinned for each driver call.
Scratch is capped at 64 MiB, input shapes are validated, and dispatches are split
at approximately one million observation/candidate evaluations so the display
GPU can schedule map work between tiles. Queued requests check cancellation
every 10 ms; active transfers are drained before returning on cancellation. The resource budget
does not promise a deadline for a malfunctioning external driver.

CPU improvements are enabled regardless of GPU preference:

- Reuse the immutable common coarse grid and bounded heap-index scratch.
- Specialize single-intercept scoring while retaining the same two-pass
  arithmetic, weights and observation order.
- Reuse reference refinement buffers and fast-mode single-point output.
- Reuse fixed internal SQLite statements for batch version validation and saves.
  Arbitrary SQL-browser queries continue to use transient statements.

Fast-fit allocations on the fixed 50-observation workload fall from about
335 KB / 367 allocations to **86 KB / 19 allocations**. Reference-fit allocation
falls from 2.31 MB to about 346 KB. Three repeated microbenchmarks are recorded;
these allocation figures do not imply a similar reduction in total survey RAM.

## Cache provenance and rollback

The old CPU reference scope and `fast` scope remain unchanged. GPU scopes use
`opencl64-reference-v1` and `opencl64-fast-v1`; their algorithm/version token is
included in estimate and preparation namespace keys alongside the existing
observation filters, fit mode and grouping. Per-target input versions remain
validated at commit. GPU result metadata records actual GPU/fallback grid use.
Unknown solver identifiers are rejected rather than aliasing reference caches.

The first GPU selection prepares an independent set of saved results. No
observations or existing CPU estimates are rewritten. Switching GPU off selects
the original CPU namespace again. Snapshots retain both histories. An oversized
or unavailable GPU calculation can produce a clearly tagged CPU fallback in
the GPU namespace; it never overwrites a CPU reference estimate.

## Complete preparation results

Windows amd64, Core i7-10700K (8 cores / 16 logical processors), Go 1.27.3,
eight Go scheduler slots. Each preparation row is one complete fresh sweep,
including normal open/startup, namespace work and durable saves. No synthetic
saved fits or reused checkpoints are counted. No compilers, tests or competing
benchmark jobs ran alongside the measured sweeps. Filesystem cache is
uncontrolled; full sweeps are single trials rather than confidence intervals.

| Dataset / algorithm | Compute backend | Workers | Complete targets | Fitted | Unavailable | Seconds | Targets/s |
| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: |
| 10M source scans, fast, previous phase | CPU | 8 | 199,998 | 199,998 | 0 | 435.89 | 458.82 |
| Same large sweep, Phase 5 | CPU | 8 | 199,998 | 199,998 | 0 | 372.68 | 536.65 |
| Same large sweep, Phase 5 | GPU float64 | 8 | 199,998 | 199,998 | 0 | **176.81** | **1,131.14** |
| 175,447 real observations, reference | CPU | 2 | 39,066 | 7,141 | 31,925 | 120.29 | 324.76 |
| Same real reference sweep | GPU float64 | 2 | 39,066 | 7,141 | 31,925 | **36.31** | 1,076.03 |
| Same real observations, fast | CPU | 2 | 39,066 | 7,141 | 31,925 | 26.18 | 1,492.24 |
| Same real fast sweep | GPU float64 | 2 | 39,066 | 7,141 | 31,925 | **17.27** | 2,261.69 |

The large fixture has 9,999,900 accepted observations after the established
100 zero-pair exclusions. All 199,998 surviving targets produce fitted locations.
Phase 5 CPU tuning improves this sweep **1.17×**. GPU improves it a further
**2.11×**, or **2.47×** relative to the preceding phase's CPU run. The older
two-worker fast baseline was 40m06s; the new tuned GPU profile is about 13.6×
faster, including all concurrency and preparation changes across both phases.

Matched real reference preparation improves **3.31×** with GPU. Real fast
preparation improves **1.52×**. Its many unavailable checkpoints and database
work limit the benefit of faster numerical scoring. Unavailable targets are not
counted as fitted locations: real-reference fitted-location throughput is about
59/s on CPU and 197/s on GPU, while the large sweep's entire 1,131/s is fitted.

Every complete CPU/GPU numeric-result hash matches its corresponding algorithm.
Validation normalizes only copies of scope/provenance metadata: GPU identifiers
map to the corresponding CPU algorithm, and the acceleration diagnostic field
is excluded. Stored GPU scopes are never rewritten. All identity, eligibility,
count, coordinate, residual and other result values must still agree. CPU results
also retain the previous raw saved-result hashes. All completed databases pass
integrity checks with no pending work. GPU sweeps report actual device use and
zero fallbacks. The complete GPU workspace reopens in 4.31 seconds with zero
scoring calls and unchanged checkpoints.

Large process working sets are **2.24 GiB CPU** and **2.35 GiB GPU**, including
the OpenCL driver's host memory and excluding the WebView. GPU VRAM is not
measured. Loaded observations still dominate RAM, so the per-fit allocation
improvement mostly reduces allocation churn. The storage decision is unchanged.

## Solver quality and conditional donor candidate

The before profile attributes about 90% of sampled reference CPU time to
candidate scoring, including 53% to logarithms. It is a separate profiled
eight-worker run, excluded from the matched two-worker headline timings.

A count-stratified real-survey comparison computes the same 128 identities in
sample and full-observation modes, without saved-fit reuse. Each mode produces
56 fitted pairs and 72 matching unavailable outcomes. Fast is about 8× faster
than reference in this single-worker subset. Fifty fitted pairs land at exactly
the same point; p95 shift is 1.46 m, and the maximum is **85.19 m**. The largest
shift has almost identical residual RMS (4.50396650584 vs 4.50396650588 dB),
consistent with ambiguous geometry. Agreement with a reference optimizer is
not a surveyed AP accuracy measurement.

GPU reference matches CPU reference, and GPU fast matches CPU fast, across every
field after scope/provenance normalization in both modes. The GPU preserves
each algorithm's existing behavior; it does not remove fast-mode ambiguities.

Forty additional known-position cases cover loop, corridor, colinear and area
geometry, noise, weighted observations, multiple radios and boundaries. GPU
variants match their CPU counterparts exactly in final coordinates and SSE.
Noise-free well-conditioned loop cases must locate the known AP within 1 m.
Noisy and ambiguous cases deliberately retain large errors: reference truth-error
p50/p95/max is 5.06 / 290.38 / 428.53 m. Fast's largest reference shift here is
35.79 m. These cases prevent an optimizer-agreement test from being presented
as proof that the radio model identifies the true position.

The donor multistart-pattern/Welford search is isolated in benchmark-only test
code, using the same prepared observations/weights. It reuses two scratch vectors
and its final objective is rescored with the production CPU arithmetic. It takes
0.066 s over these 40 cases versus 0.545 s for CPU fast, but shifts from reference
by p95 114.62 m and maximum 178.86 m. Its median truth error is 7.22 m versus
5.06 m for reference, with similarly large ambiguous-case tails. This limited
pilot does not establish a safe general replacement or real-drive parity, so
the candidate has no production dispatch, setting or cache namespace.

The standalone GPU screening pilot also tests float32. It is faster but differs
around 1e-6 relative in scores, versus around 1e-15 for float64 on this hardware.
It is not shipped. Kernel-only gains of 22–46× for float64 include upload and
readback but are not whole-solver or application throughput claims.

## Map behavior under sustained GPU preparation

The real Wails/WebView2 probe uses a fresh 10M-row workspace, real calculated
locations and 499,984 displayed sightings. Camera auto-fetch, renderer listeners
and native commit delivery remain enabled. Over 900 pan frames, p50/p95/max gaps
are **17.5 / 18.1 / 18.6 ms**. Preparation saves 4,559 locations during the trace;
pause joins in **7.3 ms**. Unchanged sighting data survives idle fit updates.
These are frame-pacing measurements, not GPU timer-query execution times.

The first diagnostic exposed AP-buffer clearing at request start. A live refresh
could clear APs and then be cancelled by the next pan before a replacement page
arrived. Same-filter pan/live loads now retain the last valid AP data until a
current first page arrives. Filters, solver identity and observation revision
still retire stale data, and cancelled/late pages remain rejected. The accepted
rerun records **zero empty AP frames**, and 4,559 APs after the view settles.
The first diagnostic is retained and excluded as proof of AP visibility.

Browser checks calculate/display all 64 fresh fixture APs in each of four
independent namespaces, verify the new preference after reload, and switch back
to CPU reference. Metadata contains four completed scopes with 64 estimates each.
Admin controls and GPU map screenshots were visually inspected.

## Remaining opportunities and phase exit

The large GPU run spends 76.18 summed seconds in commit coordination, while
whole preparation takes 176.81 seconds. Stages overlap; this is not an additive
CPU-time breakdown. Further bulk version/checkpoint operations, commit-size
experiments and profiling the SQLite writer are useful next targets. The
preceding phase also measured a roughly 4.6-second first map-index build, which
remains a separate cold-start opportunity. No durability setting is weakened.

F01 passes reference/Python parity without skipped prerequisites, real input
comparisons and known-position tests. F02 retains the donor as a measured
benchmark-only candidate. F03 preserves old CPU provenance and adds versioned GPU
namespaces. F04 delivers measured CPU/GPU allocation, queue and statement tuning
with responsive map/cancellation behavior. F05 keeps reference default, fast
optional and GPU experimental/off by default. Platform-native release validation
outside Windows remains Phase 6 work.

The full Go suite, Python/NumPy parity, vet, focused races including real hardware
scoring, ten JavaScript map tests and four benchmark-safety tests pass. Core
fallback builds pass for Linux amd64 and macOS arm64. Original scan and generated
input hashes remain unchanged. Raw real SSIDs/MACs/coordinates stay in the owned,
ignored run; committed quality evidence uses aggregate results and hashes.

## Evidence and reproduction

[phase5-summary.json](../benchmarks/baselines/phase5-summary.json) contains full
sweep commands, binary hashes, samples, integrity/numeric hashes, working sets,
kernel pilot results, synthetic truth cases, allocation trials and native/browser
reports. Owned raw fixtures and profiles remain in
`build/benchmarks/phase5-z_lkbpw0`; source surveys are preserved.

```powershell
$env:GOTOOLCHAIN='go1.27.3'
go build -trimpath -o build/benchmarks/fullprep-phase5.exe ./cmd/fullprepbench
go build -trimpath -o build/benchmarks/solvercompare-phase5.exe ./cmd/solvercompare
go build -trimpath -tags desktop,production -o build/SignalAtlas-phase5.exe .
go build -trimpath -tags desktop,production -o build/benchmarks/nativebench-phase5.exe ./cmd/nativebench
python benchmarks/phase5.py --root build/benchmarks/phase5-z_lkbpw0
# Hardware tests fail, rather than skip, if explicitly requested without a GPU:
go test -tags gpuintegration ./internal/atlas -run '^TestOpenCL' -count=1 -v
# Native driver: benchmarks/phase5-native.js; browser: phase5-smoke.cjs.
# Both require fresh owned fixture copies as recorded in the evidence.
python benchmarks/phase5_checks.py --root build/benchmarks/phase5-z_lkbpw0
```
