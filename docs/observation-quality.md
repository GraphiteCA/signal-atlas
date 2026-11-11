# Rejecting spurious observations: measured options

Recommendation: add an **optional, nondestructive quality pass** for suspect fits.
Start by distinguishing physical radios/sites from SSID names, then combine
reported GPS quality with consensus fitting. Keep the current fast preparation
as the default and preserve raw scans. This commit includes an aggregate audit
and tested prototypes; it does not enable new filters or change existing fits.

## Hidden SSIDs use addresses, not the hidden label

The full benchmark catalog contains **952,568 empty-SSID targets**, each keyed
as `h:<uppercase BSSID/MAC>`, alongside 507,993 named targets. All hidden keys in
that copy match their individual MAC addresses. Background preparation and
viewport-priority scans fetch hidden observations by SSID/MAC pair, not by the
shared `(hidden SSID)` display text. Band identity remains part of each radio key;
shared-band fitting is allowed only within the same hidden BSSID.

A follow-up fixed the legacy/manual selection helper, which previously grouped
blank names together when several hidden radios were selected. Both Go and the
Python reference now partition hidden selections by address, and direct fitting
rejects mixed hidden BSSIDs. Version checks for shared-band hidden groups use the
actual member address rather than a display string such as `1 MAC address`.
Regression tests cover 1,000 hidden addresses, distinct towns, same-address bands,
separate fit inputs and direct-fit rejection. The already MAC-scoped background
path keeps its existing saved fit identities.

A literal imported SSID such as `Hidden`, `unknown` or `(hidden SSID)` is a
nonempty name unless the importer knows it is an exporter-specific placeholder.
Do not globally rename such strings to empty: they can be real advertised names.
A future source-specific normalization policy should preserve raw values and
explain its classification. Likewise, a reused/moved BSSID still needs session
or site analysis; matching an address is not proof of permanent co-location.

## What the actual survey indicates

The aggregate audit covers all 5,753,026 scans on an owned survey copy:

| Indicator | Count |
|---|---:|
| Reported GPS accuracy worse than 25 m | 2,197 |
| Worse than 50 m | 973 |
| Worse than 100 m | 446 |
| Worse than 250 m | 43 |
| Named SSIDs with at least four scans | 124,555 |
| Of those, raw receiver positions spanning over 20 km | 34,940 |
| SSID/MAC pairs with at least four scans | 248,461 |
| Of those, spanning over 20 km | 1,209 |
| Multiple-MAC SSIDs spanning over 500 m | 52,910 |

All rows in this copy report positive accuracy. A 100 m accuracy gate alone
would exclude only 0.008% of scans. **Inference:** repeated/shared SSID names and
multiple sites are likely a more important issue than explicitly poor GPS accuracy
for this dataset. Extent counts are indicators, not proof of bad observations or
counts of recoverable fits. They use raw history and a projected maximum-axis bounding span, not the
production sampler or date filters. Actual AP coordinates are not known ground truth here.

An SSID can describe many physical APs. Current shared fitting assumes radios
with the same name are co-located, with independent signal-power intercepts.
That assumption needs testing before combining evidence. A network distributed
among homes, shops or hotspots cannot be accurately represented by one location.
Receiver-position clusters are also not themselves AP locations: one legitimate
AP can be heard from disconnected routes.

An immediate existing option is **Admin → Locations to calculate → Individual
radios**, paired with **Estimates → Individual radios** on the map. This avoids
the shared-SSID co-location assumption; it does not reject bad observations or
resolve a moved radio by itself.

## Prototypes and known-location measurements

The deterministic diagnostic uses eight trials for each of six scenarios,
96 receiver observations per case and 2 dB signal noise. Contaminated cases alter
eight observations. It compares seven policies using the actual Go fast solver.
The combined candidate does the following:

1. Exclude fixes reporting accuracy worse than 100 m; retain missing/unknown
   accuracy rather than treating it as zero-quality evidence.
2. Flag substantial separated receiver components for review instead of forcing
   one shared position. The prototype uses a 300 m neighbor graph and requires a
   90% dominant component. It does **not** drop small isolated components here.
3. Try the full fit plus twelve deterministic twelve-observation hypotheses.
   Score consensus using a robust median power intercept per radio, an 8 dB
   residual threshold and at least 70% consensus, then refit accepted observations.

This is a RANSAC-style candidate inspired by [consensus regression](https://scikit-learn.org/stable/modules/generated/sklearn.linear_model.RANSACRegressor.html),
not the scikit-learn implementation or a proven success probability for arbitrary
Wi-Fi data. Thresholds are experimental, not calibrated probabilities.

| Synthetic scenario | Baseline median position error | Combined candidate |
|---|---:|---:|
| Clean measurements | 2.90 m | 2.90 m |
| Eight +35 dB RSSI spikes | 76.88 m | 4.14 m |
| Eight false GPS jumps, still claiming good accuracy | 940.82 m | 1.86 m |
| Eight poor GPS fixes reporting 250 m accuracy | 391.22 m | 2.86 m |
| Eight legitimate distant readings | 2.39 m | 2.39 m |
| Two physical sites sharing an SSID | No single correct position | Flagged in all eight trials |

In these fixtures the combined candidate removed all eight spurious readings
in each contaminated case and removed no legitimate readings. The independent
known-position assertions require error below 20 m and correct ambiguity handling.
This is encouraging synthetic evidence, not a measured accuracy guarantee for the
private survey, arbitrary route geometry, severe multipath or moved/mobile APs.

CPU fit cost was around 70–74 ms versus about 20–22 ms for the baseline, roughly
3.5×. These are small-shape diagnostic timings, not production GPU measurements
or whole-survey preparation rates. Applying the extra work indiscriminately
would conflict with the preparation-performance goal.

## Why simpler policies are insufficient

**GPS accuracy alone:** cheap and useful when honestly reported, but misses bad
coordinates that claim good accuracy. Most scans in the actual survey already
claim better than 100 m. Implement it as a configurable fit exclusion with counts,
not an import-time deletion rule.

**Spatial deletion alone:** the 300 m graph successfully removed the synthetic GPS
jumps, but also discarded all eight legitimate distant readings in that control
scenario. Its threshold describes receiver sampling density, not radio range.
[DBSCAN](https://scikit-learn.org/stable/modules/generated/sklearn.cluster.DBSCAN.html)
offers explicit density/noise handling, but parameter selection and different
sampling densities remain material. Do not run a global all-pairs clustering
algorithm across millions of observations; use bounded per-radio windows and
spatial postings.

**Huber weighting alone:** four fits with a 6 dB Huber-style IRLS weight reduced
the spike error only from 76.88 to 64.63 m and did little for poor GPS fixes.
[Robust least-squares losses](https://docs.scipy.org/doc/scipy/reference/generated/scipy.optimize.least_squares.html)
reduce outlier influence, but a biased initial position/search origin can trap
this particular iterative procedure. A robust global objective or better
hypotheses matters. This is a result about the tested procedure, not a conclusion
that every Huber implementation will fail.

**Consensus alone:** handled RSSI spikes and GPS jumps and retained distant scans,
but struggled with the poor-GPS case and could accept a shared fit across two
sites. The GPS gate and ambiguity checks materially improved the combined
candidate. Strongest-signal-only filtering is especially unsuitable: a spurious
high RSSI observation can dominate the existing strongest-reading sample.

**Speed/jump rejection:** potentially useful only with trustworthy capture-time
and source/session identity. This app currently retains `FirstSeen` and a derived
date; the audit did not establish those as reliable per-GPS-fix capture times.
Do not infer vehicle speed by subtracting arbitrary same-SSID timestamps.

## Proposed implementation order

1. Add per-BSSID/band/site diagnostics and a preview of conflicting shared SSIDs.
   Offer separate fits for distinct physical radios rather than deleting other
   valid APs or averaging their positions. Flag same-radio history that may span
   a moved AP or multiple sessions.
2. Add optional GPS-quality exclusions before bounded strongest/hash sampling.
   Show total/retained/excluded counts, threshold and reasons; keep equator and
   prime-meridian observations valid. Preserve existing finite/range and 0,0 checks.
3. Add the consensus pass only for selected or suspicious fits, such as large
   residuals or boundary-limited positions. Recheck geometry and inlier support
   after rejection; defer ambiguous fits rather than inventing confidence.
4. Give the policy/version/thresholds distinct cache and preparation namespaces,
   and expose raw versus filtered positions plus exclusions in AP details. Robust
   fitting cannot reuse the current exact coordinate-aggregation statistics to
   identify individual within-position RSSI outliers; filter raw rows first or
   retain additional sufficient data. Keep CPU/GPU comparisons explicit.
5. Validate against known stationary AP coordinates and held-out routes/sessions,
   reporting median/p95 error, false rejection, coverage, runtime and memory.
   Expand tests to sparse, collinear, multimodal and mobile cases before considering
   a default change.

Reproduce the aggregate audit with `cmd/qualityaudit -data <owned-copy-parent>`.
Set `SIGNAL_ATLAS_QUALITY_REPORT` to an owned benchmark directory and run
`go test ./internal/atlas -run '^TestObservationQualityCandidates$' -count=1`.
The report file must be new. No identities, private records or source logs are
written to committed evidence. See [measurement JSON](../benchmarks/baselines/observation-quality.json).
