# Signal Atlas · Wi-Fi lateration

Explore large, multi-SSID Wi-Fi surveys, filter independent BSSIDs and frequency
bands, and estimate transmitter positions. Includes a local SQLite-backed
explorer for hundreds of thousands of observations, optional satellite imagery,
5–300 metre planning circles, and a portable single-network report.

## Large surveys: start here

Python 3.10+ with SQLite/RTree support and NumPy are required. From this folder:

```powershell
python -m pip install -r requirements.txt
python survey.py init
python survey.py import your-survey.csv
python survey.py serve --port 8765
```

SQLite is the source of truth. Import a CSV once, then run `python survey.py serve`
to reopen the same observations, imports, map context and saved estimates. No
CSV reread or generated JSON report is needed to reopen the viewer. The default
database is `outputs/survey.sqlite3`; choose another with the global option:

```powershell
python survey.py --database my-survey.sqlite3 init
python survey.py --database my-survey.sqlite3 import inputs
python survey.py --database my-survey.sqlite3 serve
```

Open **http://127.0.0.1:8765/** and use **Import CSV** to append more observations.
The file is streamed to the local server and imported in a background job. The
server binds only to this computer. Leave the Python process running while using
the explorer; Ctrl+C stops it. It does not upload your observations to a cloud
service. This viewer is separate from the portable `file://` report.

Bulk imports can also be run from the command line, before starting the viewer:

```powershell
python import_survey.py inputs --database outputs/survey.sqlite3
python import_survey.py first.csv second.csv --database outputs/survey.sqlite3
```

`import_survey.py` and `serve_survey.py` remain compatible entry points; `survey.py`
is the primary interface. Existing Signal Atlas databases migrate in place to
schema version 5 without reimporting their data. Unrelated SQLite databases are
rejected rather than populated with new application tables.

### The SQLite artifact

The database banner names the connected store and shows its persistent totals.
**Save SQLite database** downloads the **whole database**, including unselected
SSIDs, import provenance, spatial indices, saved estimates, pinned date comparisons, and local map context.
It uses SQLite's backup API so committed data in WAL is included consistently.
Copying only a live `.sqlite3` file yourself may miss committed WAL content.

```powershell
python survey.py status
python survey.py backup saved-survey.sqlite3
python survey.py --database saved-survey.sqlite3 serve --port 8767
```

Backups refuse to overwrite existing files. Original CSVs and report JSON files
are not required to use a saved database. The SQL views `network_catalog`,
`transmitter_catalog` and `radio_catalog` expose SSIDs, SSID/MAC pairs, and
band-specific radios for inspection in another SQLite client:

```sql
SELECT ssid, mac, bands, count
FROM transmitter_catalog
WHERE ssid = 'ExampleNet'
ORDER BY count DESC;
```

Existing `basemap.geojson` context beside the database is adopted into SQLite
once. To explicitly replace or add context later:

```powershell
python survey.py basemap outputs/basemap.geojson
```

Source metadata and attribution stay with the stored GeoJSON. Satellite API keys
remain outside the database, so a SQLite backup never contains the imagery key.

Folder imports recursively discover CSV surveys, including `.CSV` extensions,
and overlapping input paths are deduplicated before import.
Ordinary CSVs and WiGLE exports with a `WigleWifi-...` metadata line are accepted.
Required columns are `MAC`, `SSID`, `RSSI`, `CurrentLatitude`, and
`CurrentLongitude`. If `Type` is supplied, only `WIFI` rows are imported; Bluetooth
and cellular rows are counted separately and excluded from AP estimation.
Importing a file with the same SHA-256 again is a no-op. New files append to the
database. Each file's import is atomic; malformed measurements roll back that
file by default. Choose **Skip invalid rows** in the viewer or `--skip-invalid`
on the CLI to explicitly skip them and receive a count. Importing additional
data clears cached estimates so changed histories are never shown as old fits.

The database, temporary uploads, raw `inputs/*.csv` files, and SQLite WAL files
are ignored by Git. The small synthetic ExampleNet CSV is the tracked demo.

### Select networks, radios and bands

- **Nearby APs** is on by default. The map shows one shared estimate per named SSID whose fitted
  centers fall inside the viewport, even with no SSIDs selected. Panning updates
  the overview. Matching exact fits take precedence over sampled versions.
  At zoom 16 and above, the viewer also checks up to 60 nearby candidates and
  computes missing AP estimates in batches of eight. The default model fits one
  shared location per named SSID using up to 20 radio groups. Hidden SSIDs are
  grouped by MAC. Check **Estimate individual radio locations** to calculate
  separate fits per radio. Observations missing band data join the same MAC’s
  known band when it has exactly one. Dates, bands, signal and search still apply.
  Saved comparison layers also start hidden; check a layer or use **Show** to display it.
- Search SSIDs to filter both the list and map; clearing the search restores the wider selected view. Use **Only** for one network, then check others to compare them.
  **All** and **Clear** work across the entire database, including SSIDs outside
  the current list page. Expand an SSID's arrow to see its MAC addresses directly
  underneath. MAC children load from SQLite on demand, have their own search and
  pagination, and show the bands observed for that MAC.
- Check or uncheck MAC children to refine that SSID. Other selected SSIDs keep
  their own MAC choices. Parent checkboxes show partial selection when only
  some MACs are selected. Checking a MAC under an unselected SSID selects just
  that MAC, not every transmitter advertising the name.
- A MAC's **Locate** button focuses that SSID/MAC and starts its location job.
  It includes its observed bands as distinct radio fits, respecting band filters.
  The separate radio list can narrow the selection further to one band.
- Each survey layer identifies its SSID, BSSID/MAC and band: **2.4 GHz**, **5 GHz**,
  **6 GHz**, Other, or **Band unavailable**. Different BSSIDs under one SSID stay separate.
  Even a repeated BSSID is separated if it is observed on different bands.
- Band is derived from `Frequency` (MHz, GHz, kHz or Hz accepted) or an explicit
  `Band` column. A conflicting declaration stays **Band unavailable**. Channel alone, the
  SSID text, and neighboring MAC addresses are not used to guess a band. The
  supplied WiGLE exports omit usable frequency/band information, so their band
  displays as **Band unavailable** until you supply a known band. In all eight supplied files, `Channel` is `-1` and frequency is absent, so their real 2.4/5 GHz bands cannot be inferred from those exports.
- Filter bands, minimum RSSI, date ranges and individual radios. Color points by
  signal strength, SSID or band. Per-network colors are stable between selections.
- Dense map views show a bounded set of aggregate cells at their mean receiver
  coordinates. Counts represent all matching observations; those centroids are
  **display summaries**, not measured receiver locations or AP estimates. Click
  a cell or zoom in to inspect its constituent records. The ledger pages through
  the exact stored observations without sampling.

### Independent estimates and 5-metre circles

Locate directly from SQLite without a CSV or browser:

```powershell
python survey.py locate --ssid ExampleNet --mac 02:5A:00:00:01:00
python survey.py locate --ssid "Shared network" --mac 02:00:00:00:00:01 --band 5
```

Repeat `--mac` or `--band` to select multiple values. Results are persisted in
SQLite, so the viewer sees the same cached candidates. `--output result.json`
is an optional export, not a required database dependency.

### Missing band metadata

Expand an SSID, then use **Set missing band** under a MAC to declare **2.4 GHz**,
**5 GHz**, or **6 GHz** when you know that radio's band. The assignment applies
only to that SSID/MAC's observations without usable band metadata; recorded
frequency and explicit-band measurements remain authoritative. It adds a
**Manual** badge so declared bands can be distinguished from recorded bands.
Choose **Not assigned** to restore the missing-band state.

Assignments live in SQLite, survive backups, and apply to future imports that
lack band metadata. Changing an assignment refreshes band tags, filters and
radio grouping, and invalidates live fit caches. Original CSV files are not
rewritten. Historical pinned comparisons retain their original metadata.
The SSID row aggregates the bands seen across its radios; each expanded MAC
shows its own recorded or assigned bands. No bands are guessed from adjacent
MAC addresses or an SSID name.

### All observations, selected dates or date ranges

**Observation dates** applies to both the displayed observations and new fits:

- **All recorded dates** uses the entire imported observation history for the
  selected SSIDs/MACs/bands, subject to the displayed minimum-signal filter.
- **Date range** restricts the fit itself to the From/Through dates, inclusive.
  Either boundary can be left blank for an open-ended range.
- **Specific survey dates** accepts multiple, nonconsecutive dates from the
  stored surveys. A specific-date selection with no dates selected matches no
  observations. Dates are taken from the recorded `FirstSeen` date, without
  timezone reinterpretation, rather than the filename or import date. Undated
  measurements are included in All mode but excluded by a date restriction.

**Estimate data → Every matching observation** is the default. It uses every
measurement matching the SSID/MAC, band, signal and date filters; there is no
256-row cap. Repeated measurements at exactly identical GPS coordinates are
combined using counts, mean RSSI and within-location squared residuals. This
preserves the full least-squares objective, up to floating-point rounding,
without duplicating coordinate calculations. GPS coordinates are not rounded
into approximate bins. Candidate calculations are chunked and cancellation is
checked during model evaluation. Fits with many unique positions can take
longer; the background job keeps the viewer usable.

**Fast sample · 256 per radio** is an explicit optional approximation. It retains the
previous deterministic mixture of strong and spread observations, drawn only
from the same selected dates and filters. The result shows its used and matching
counts and identifies the sample mode.

SQLite caches each radio's date window/selected dates, signal threshold and fit
mode separately. Changing dates never substitutes a full-history result, and
running a date-restricted fit does not overwrite an all-history fit. The prior
schema's computed results remain preserved as legacy samples but are not reused
as new exact or date-scoped fits. Observation and import records are unchanged.

```powershell
# Every matching observation across the whole imported history
python survey.py locate --ssid ExampleNet --mac 02:5A:00:00:01:00
# Inclusive date window, using every matching observation in that window
python survey.py locate --ssid ExampleNet --date-from 2026-01-01 --date-to 2026-09-30
# Nonconsecutive survey dates (repeat --date)
python survey.py locate --ssid "Shared network" --date 2026-09-02 --date 2026-09-24
# Explicit fast approximation within a date window
python survey.py locate --ssid "Shared network" --date-from 2026-09-01 --fit-mode sample
```

At least four matching observations and three distinct receiver positions are
required per radio/band. A narrow date window may not contain enough data; the
viewer reports that instead of quietly falling back to unrestricted history.

**Locate selected MACs / radios** runs a cancellable background job using the
chosen **Location model**. The viewer defaults to a shared AP location per SSID.
Check **Estimate individual radio locations** to add separate `(SSID, BSSID, band)`
fits. The revealed **Location model** defaults to **Individual radios only**;
choose **Shared + individual radios** to compare both. The selected filters
choose which observations and radios to fit.
Each fit uses the observations within the **selected dates and filters**.
Candidates and their circles are
shown together, and **Locate** zooms to an individual result. Calculated markers,
planning circles, and comparison links use opaque dark and white outlines for
contrast over street, night, and satellite maps. Labels sit above the markers
on an opaque dark background. Candidate responses
are bounded to 200 radios (500 maximum through the API); select fewer SSIDs or
radios when a broad selection has more candidates. The candidate list displays
up to 60 at once, and indicates when results are capped.

Every matching observation is used by default. Only the optional fast mode
uses the 256-observation mixture of 32 strongest records and up to 224 records
sampled across the matching history; overlapping samples are removed.
Sampling can affect the result. Fits assume n=3, constant radio power,
and a stationary transmitter. Sparse, coincident, excessively dispersed or
boundary-limited histories need additional interpretation. Radios with fewer
than four observations are not fitted. Jobs continue in the background and can
take minutes for many radios; they do not block map queries.

The radius slider and numeric field accept **5–300 metres**, starting at **30 metres**. The portable estimator CLI also defaults to 30 metres; previously generated reports keep their saved radius. This is a chosen
planning radius, not a confidence interval or evidence of 5-metre localization
accuracy. **Export visible points** downloads the currently drawn observations
or aggregate cells, selected cached candidates, and their geodesic search-area
polygons as GeoJSON. Visible pinned comparisons and displacement connectors are included. It does not export every row in the database.

### Live selection and shared-location fits

**Refine the view** is a horizontal blade directly below the map. By default,
**Auto-update estimates** calculates missing candidates when the SSID, MAC,
radio, band, signal or date selection changes. Existing fits for unchanged
radios/dates are reused. To avoid launching hundreds of fits while exploring a
large database, automatic jobs require at most **20 eligible radio/band groups**.
For broader selections use **Locate selected MACs / radios**, or narrow the
selection. Cancelling a job pauses automatic updates for that selection until
you change the selection, toggle automatic updates, or locate manually.

Each listed radio shows its own band. Candidates also list **MAC seen bands**
and **SSID seen bands** across the full stored history, including bands outside
the current filter. Missing source frequency data appears as **Band unavailable**, with an explanatory tooltip.

**Estimate individual radio locations** is unchecked by default. The map shows
one common AP location per selected SSID, fitting a separate power intercept for
each selected BSSID/band. Individual-radio candidates and rings are omitted in
this mode, including previously cached individual estimates. Selecting or
deselecting radios recalculates the shared fit. With only one selected radio,
its ordinary fit supplies the one AP candidate.

Shared fits assume the selected radios are colocated. Enable **Estimate individual
radio locations**, then choose **Individual radios only** for separate locations,
or **Shared + individual radios** to inspect both. An independent fit for an
unchanged radio stays the same. A shared fit is limited to 20 selected radio/band groups per SSID. Its exact member set and date
scope identify the cache entry, so a fit from a different radio subset cannot
be substituted. This also works from the CLI:

```powershell
python survey.py locate --ssid ExampleNet --shared
```

### Follow the selected AP

Selecting an SSID automatically zooms to its shared AP estimate as soon as the
fit is available. Selecting or deselecting radios recenters on the updated fit
while preserving the current zoom for that SSID. Ordinary pan/zoom and ledger
refreshes do not snap the map back. **Follow AP** can disable automatic
recentering; **Center AP** jumps to the current estimate, and **Fit observations**
shows the receiver measurements instead. Multiple selected APs are fitted into
the same view. Startup still selects no SSIDs and draws no AP rings.

### Compare date ranges on one map

1. Choose an SSID/MAC and the first date range in **Refine the view**. Wait for
   the estimates, give the layer a name in **Compare date ranges**, then select
   **Pin current estimates**.
2. Choose a second range and wait for its estimates. Pin it under another name.
3. Use **Fit comparison layers** to show the live and pinned candidates together.
   Each pinned layer has its own color. Connectors match the same SSID/BSSID/band
   (or the same shared-fit member set) and label the estimated separation in metres.

Up to six layers, each with up to 60 candidates, can be saved. Pins include the
original filters, dates and fit results, are stored in SQLite, and survive
refreshes, backups and new imports. They are snapshots: later imports invalidate
live caches but do not change historical pins. Saved layers start hidden on reopening the viewer. Newly pinned layers appear
immediately. Hide/show, zoom to, or remove each layer independently. Clearing the live SSID selection removes its current
points and candidates; explicitly pinned layers remain visible until hidden.
The online-map setting and imagery key are never stored in comparison filters.

Separation is displacement between model estimates, not proof of physical AP
movement. Compare like-for-like radios and receiver coverage; sparse data,
RSSI propagation, changed power and GPS error can move the fitted point.

### Satellite imagery

Choose **Satellite · Esri key** and enter an ArcGIS API key with access to basemap
services. The supported Esri World Imagery endpoint requires authentication;
see [Esri's documentation](https://developers.arcgis.com/maplibre-gl-js/maps/raster-tile-basemaps/display-multiple-basemap-layers/).
Configure any key restrictions to allow your local viewer's origin. A key is not
included with this project and may be subject to the provider's account limits
and pricing. The key stays in tab memory, is cleared from its input after entry,
and is never written to the SQLite database, survey reports, exports or Git.

Only explicitly selecting Satellite requests imagery, and those requests reveal
the viewed map area to Esri. Imagery requires internet and is not downloaded for
offline use. Provider attribution remains visible. A tile failure leaves the other imagery tiles visible and offers **Retry tiles**; a persistent authentication error needs a valid key or a different map style. Satellite request construction and failure
handling were tested with locally intercepted test tiles; real imagery needs
your valid key and has not been validated with an account in this workspace.

**Live street · OpenStreetMap** is now the default map style. It requests tiles
for the viewed area as you pan and zoom, including beyond the saved ExampleNet
excerpt. The provider receives the viewport and IP address, but no SSIDs, MACs
or survey records. Normal browser caching and referrer headers are preserved,
and attribution remains visible. There is no bulk download, proxy, automatic
retry loop or offline tile archive. A single tile failure no longer disables
the whole live layer; successful tiles remain visible and **Retry tiles**
reloads the current viewport on demand. The source supplies detail through
zoom 19, enlarged at zoom 20–22 instead of requesting nonexistent tile levels.
This mode follows the [OpenStreetMap tile usage policy](https://operations.osmfoundation.org/policies/tiles/)
and remains subject to provider availability and browser privacy settings.

The **Offline excerpt · night/street** styles use the small map context stored
inside SQLite and make no tile requests. They cannot add new streets outside
that saved area. **Survey canvas** also stays local. Without Leaflet/internet,
the explorer retains its canvas, selection, filters and local database queries.

### Scale check

`tests/benchmark_survey.py` generates synthetic data without touching your real
survey. The recorded run in `outputs/benchmark-300k.json` imported **300,000 rows,
100 SSIDs and 200 radio groups**. With Python allocation tracing enabled, import
took 62.1 seconds; summary and map queries took 0.33 and 0.58 seconds, respectively.
The map response contained 1,910 marks representing all 300,000 rows. Peak traced
Python allocations during import were 3.51 MiB; this **excludes SQLite/NumPy
native allocations** and is not a total-process RAM measurement. One bounded
radio fit in fast-sample mode took 1.28 seconds. Exact fits can take longer;
timings vary by hardware and survey geometry.

```powershell
python tests/benchmark_survey.py --rows 300000 --output outputs/benchmark-300k.json
python -m unittest discover -s tests -v
```

The included demo is synthetic: one made-up network, ExampleNet, with four MAC addresses in a public
forest. Its default run fits **48.4491752, 2.6681101**, using exponent 3 and
separate power offsets for the four MAC addresses. The largest leave-one-out
shift is approximately **5 metres**, illustrating why the model's fine grid
resolution should not be read as physical accuracy.

## Portable single-network report

Python 3.10+ and NumPy are the only Python requirements. From this folder:

```powershell
python -m pip install -r requirements.txt
python estimate_ap.py demo-survey.csv --output outputs/ap_estimate.json
python download_basemap.py outputs/ap_estimate.json --output outputs/basemap.geojson
python plot_observations.py outputs/ap_estimate.json --output outputs/demo-map.html
```

Open `outputs/demo-map.html` in a browser. No web server or API key is
needed for local map styles. **By default the report makes no raster tile requests.** Street and building
geometry is embedded from a small, attributed local OpenStreetMap excerpt.
Leaflet loads from its pinned CDN for pan and zoom; if that library is
unavailable, the same local geometry renders in the offline SVG view.

`download_basemap.py` is an optional, explicit one-time data import from the
public Overpass API. Its query discloses the approximate 800 × 800 metre map
area to that service, but does not send MAC addresses, SSIDs or RSSI values.
It requests vector features, not raster tiles, and reuses the saved file on
subsequent runs. No automatic retries or background map downloads occur.
Skip this command if you do not want that request: the exact survey grid
still works, with street-map options disabled until an excerpt is supplied.
`--refresh` explicitly replaces a cached excerpt. `--half-width-m` is limited
to 100–500 metres to keep each request small. The excerpt includes standalone
tagged ways; multipolygon relations are not assembled, so this is a limited
local context map, not a complete or navigable geographic database.

The generated HTML embeds the observations and model results. It can be moved
or shared as one file. Sharing it also shares the coordinates and MAC addresses.

## Files

- `estimate_ap.py`: CSV validation, metre-coordinate model, profile least-squares
  search, sensitivity analysis, JSON and GeoJSON output.
- `download_basemap.py`: optional small vector-data import, with caching.
- `plot_observations.py`: creates the interactive HTML and the static SVG using
  `templates/survey_map.html`, `templates/local_basemap.js`, `templates/satellite.js`
  and `templates/report_satellite.js`. Copy these template
  files with the script when reusing it. By default it uses `basemap.geojson`
  beside the estimate report, or select a local file with `--basemap`.
- `outputs/basemap.geojson` and `outputs/basemap.overpass.json`, when present:
  local derived map geometry and its original source response, licensed under
  [ODbL 1.0](https://opendatacommons.org/licenses/odbl/1-0/), © OpenStreetMap contributors.
- `outputs/ap_estimate.json`: fitted location, per-radio power intercepts,
  per-observation residuals, settings, source checksum and sensitivity results.
- `outputs/ap_estimate.geojson`: observations, fitted point and a geodesic
  search-area polygon. Coordinates use GeoJSON's longitude, latitude order.
- `outputs/demo-map.html`: interactive report.
- `outputs/demo-map.svg`: north-up local metre overview; intentionally
  has no street basemap. It works without any external assets.

Included PNG images are snapshots of this supplied dataset. Re-running the
plotting script regenerates HTML and SVG, not the browser-captured PNG previews.

## Explore

- Pan and zoom the geographic map; choose Night, Street, or Offline survey grid.
- Select MAC addresses, dates and minimum RSSI to filter the map and ledger.
  **These are display filters. They do not refit the estimator.**
- Click a map point or a ledger record to inspect its exact coordinates, receiver
  accuracy, signal strength, distance from the fitted point, and RSSI residual.
  The ledger makes tightly overlapping observations individually accessible.
- Adjust the planning radius down to 5 metres. It starts at 100 metres and is not a confidence
  interval. Export GeoJSON includes the current radius and visible observations;
  the JSON download preserves the original estimation report and its settings.
- Toggle receiver GPS accuracy rings, alternate path-loss/per-radio fits,
  leave-one-out positions and the model fit landscape.
- The landscape's bright regions indicate low model residuals. They do not
  represent measured coverage or a probability of the transmitter being there.
  This layer is available on the geographic map; the offline grid shows the
  points, circle and sensitivity markers instead.
- Read the observed-versus-predicted plot and expandable method notes.

The map uses colour and point size for received signal strength. No points are
jittered, and observations from sparse, separate drives are not connected into
a fictional route. Night and Street styles use the same embedded vector
geometry. Attribution remains visible. There are no live map-tile requests.
Pan and zoom remain within the local excerpt's approximate coverage; context
outside it is not available.

## Reuse with another survey

Required CSV columns: `MAC`, `SSID`, `RSSI`, `CurrentLatitude`,
`CurrentLongitude`. Optional fields include `id`, `FirstSeen`, `AccuracyMeters`,
`Channel`, `Frequency`, `source`, and `day_table`. RSSI is in dBm, GPS coordinates
are treated as WGS84 latitude/longitude, and accuracy is in metres. Missing
accuracy stays missing. Non-finite or invalid selected measurements cause an
explicit error.

```powershell
python estimate_ap.py another-survey.csv --ssid "My Network" --radius-m 100 --output outputs/my-estimate.json
python download_basemap.py outputs/my-estimate.json --output outputs/my-basemap.geojson
python plot_observations.py outputs/my-estimate.json --basemap outputs/my-basemap.geojson --output outputs/my-map.html
```

To estimate only the strongest ExampleNet radio, without the shared-location
assumption:

```powershell
python estimate_ap.py demo-survey.csv --mac 02:5A:00:00:01:00 --output outputs/b0-estimate.json
python plot_observations.py outputs/b0-estimate.json --output outputs/b0-map.html
```

Use `--help` on either script for options. `--path-loss-exponent` defaults to 3;
this is an assumption, not a calibrated property of the site. Sensitivity checks
also use exponents 2, 2.5, 3, 3.5 and 4. `--no-sensitivity` skips alternate,
per-radio and leave-one-out fits for a faster run. The primary fit still runs.

## Method and interpretation

The model is:

```text
RSSI_i = A_radio(i) − 10 n log10(max(distance_i, distance_floor)) + residual_i
```

At each candidate location, the intercept for each radio is solved analytically
as the mean of `RSSI + 10 n log10(distance)`. The location minimizes the summed
squared within-radio residuals. This allows different radios to have different
power offsets. A singleton radio contributes no location information because
its free intercept exactly fits its only observation. The script rejects data
with fewer than three within-radio contrasts or three distinct receiver
positions. These minimum checks do **not** guarantee identifiability.

The default search spans a 1 km square around the mean receiver position,
samples a 5-metre grid and refines eight separated low-error candidates at
0.25-metre spacing. This is a multi-start numerical search, not proof of a
global optimum. Refinement spacing does not imply centimetre or metre physical
accuracy. A fit near the search boundary is flagged. Increase `--extent-m` when
needed, choosing a suitable `--grid-step-m` to limit computation. Local
equirectangular coordinates use a spherical Earth approximation; surveys
spanning more than 20 km or near the poles require a different projection.

The model uses horizontal distance only. GPS accuracy and antenna height are
not fitted as extra unknowns. The SVG and map retain exact source coordinates.
The reported RMS is an in-sample RSSI residual, not metres of location error.

The combined fit assumes all selected MACs are stationary and co-located, and
that each radio's power is stable across dates. An SSID alone does not establish
any of these conditions. A shared location can describe a household or mesh
network poorly when its devices are physically separate. Use `--mac` to isolate
a device; per-radio alternatives are generated only when enough measurements
are available. Small residuals do not validate the shared-location assumption.

Receiver sampling geometry, unknown transmit power, uncalibrated propagation,
walls, antenna direction, multipath, and changes between surveys can move the
inferred position substantially. The 100-metre circle is an initial search
area, **not** a 95% confidence interval or guaranteed containment boundary.
Leave-one-out and exponent sensitivity illustrate some model dependence but do
not calibrate uncertainty. A signal-weighted receiver centroid is also recorded
for comparison; it is not treated as a triangulated location.

Reference: [Collaborative Indoor Access Point Localization Using Autonomous
Mobile Robot Swarm](https://pmc.ncbi.nlm.nih.gov/articles/PMC5854978/), especially
its log-distance RSSI model and discussion of environmental variation.
Map library: [Leaflet 1.9.4](https://leafletjs.com/reference.html).
Basemap data: [OpenStreetMap](https://www.openstreetmap.org/copyright), imported
through the [Overpass API](https://dev.overpass-api.de/overpass-doc/en/).
Local `file://` pages do not supply the web referrer required by the OSM
[tile usage policy](https://operations.osmfoundation.org/policies/tiles/), and
embedded app identification may cause additional blocks. The report therefore
renders local vector data instead of calling volunteer-run raster tile servers.

## Verify

```powershell
python -m unittest discover -s tests -v
```

Tests cover recovery of a known synthetic transmitter with different radio
powers, invariance to per-radio power shifts, the geodesic circle radius,
uninformative-radio rejection, input validation, GeoJSON coordinate order,
JSON serialization, and safe HTML embedding of arbitrary SSIDs.
