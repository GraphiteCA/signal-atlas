# Signal Atlas

A desktop app for Windows, macOS and Linux, for wardriving data too big for the browser. Its main analysis tools are:

- **Wardrive Compare**, by **[Champ](https://github.com/TheChampCodes)**. Drop WiGLE CSV logs from any logger and compare drives on the road they
  share: networks per second and per km, how long networks stay visible, speed bands, channels, a
  baseline drive with a bootstrap range and fairness checks, repeat passes, multi-node rigs.
- **Signal Atlas explorer**, by **[Graphite](https://github.com/GraphiteCA)**. Import whole CSV surveys or open SQLite databases, filter by SSID, MAC, band,
  date and signal, and fit where each network's access point is. It supports one fit per radio or
  one shared location per SSID, nearby fits as you pan, and pinned date-range comparisons.

Database administration includes duplicate-sighting cleanup and WiGLE CSV export directly on the
active survey workspace. No separate archive is required.

**Which files.** Every tool takes plain **WiGLE CSV logs** from any logger, and **WarHog's database
files**: the per-day `wardrive_YYYY_MM_DD.sqlite3` files a WarHog Core writes to its SD card, copied
to your PC, or a database saved from warhog.app. Any SQLite file whose tables have WiGLE's columns
(`MAC`, `CurrentLatitude`, `CurrentLongitude`, …) works the same way. Rows logged before the GPS had
a fix are left out where a position is needed.

It runs in its own window and does not open a browser. Your data never leaves the computer, and the
app is dark-themed throughout.

## Running it

Download your system's file from the Releases page.

| System | File | Notes |
|---|---|---|
| Windows 10 or 11 | `signal-atlas-<version>-windows-amd64-setup.exe` | Installs for you alone (no administrator prompt), with Start-menu and desktop shortcuts and an uninstaller in **Settings → Apps**. Updates install from inside the app. |
| Windows, portable | `signal-atlas-<version>-windows-amd64.zip` | Unzip and run `signal-atlas.exe`. |
| macOS 12 or newer | `signal-atlas-<version>-macos-universal.zip` | Unzip and move **Signal Atlas** to Applications. One app runs on Apple silicon and Intel Macs. |
| Ubuntu 22.04+, Debian 12+ | `signal-atlas_<version>_amd64.deb` | `sudo apt install ./signal-atlas_<version>_amd64.deb` installs it and what it needs. |
| Other Linux | `signal-atlas-<version>-linux-amd64.tar.gz` | Needs GTK 3 and WebKitGTK 4.1 (`libwebkit2gtk-4.1-0` on Debian and Ubuntu, `webkit2gtk4.1` on Fedora). |

The screens are drawn by the system's web view: WebView2 on Windows (Windows 11 has it; the
installer offers Microsoft's download on a Windows 10 PC without it), WKWebView on macOS, WebKitGTK on
Linux.

Unless a release says it is signed, the builds are not code-signed, so each system warns the first
time:

- **Windows.** SmartScreen may say the app is unrecognised. Choose **More info**, then **Run anyway**.
- **macOS.** The first open is refused. Open **System Settings → Privacy & Security** and choose
  **Open Anyway** next to Signal Atlas, or right-click the app and choose **Open**.

Files are read where they are on disk. Drop logs on the window, or use **Choose files** and
**Import CSV**, which open the system's file dialog. Exports and saved databases go through the
system's Save dialog. Each tool has a **Home** link back to the start page.

**Database administration.** Open **Admin** from the map or home page. **New database** creates a
separate survey workspace; **Open database** and recent surveys switch between existing workspaces.
The last survey opens next time. Switching safely stops its survey job while keeping
Compare open. Native file and folder dialogs handle paths in the desktop app.
**Refresh list** rechecks recent workspace folders; missing folders disappear automatically.
**Remove from list** forgets the selected workspace without deleting its files or closing it.
A forgotten workspace can be added again with **Open database**.
**Delete workspace** permanently removes the selected folder and all its contents after an
explicit confirmation. It does not use the Recycle Bin. Open another survey first to delete
the active workspace; workspaces open in another process cannot be deleted.

**Import scans** accepts multiple CSV logs, WiGLE backups, logger/WarHog databases and saved survey
SQLite files. Importing a saved database adds its scans; **Convert a saved SQLite survey** creates a
new workspace with its saved locations and comparisons. Original files stay unchanged. Rows whose
latitude and longitude are both numeric zero are excluded automatically and counted separately.
Valid equator or prime-meridian readings with only one zero coordinate stay. The survey takes Wi-Fi
sightings.

**Save snapshot** offers a fast, complete workspace copy or a portable SQLite file containing all
observations and metadata. **Maintenance → Preview 0,0 rows** finds old zero pairs before explicit
cleanup. Cleanup retains a previous revision for recovery, preserves surviving IDs and source
history, and invalidates only affected saved locations. Validation, lookup rebuilding and metadata
maintenance are available alongside it. See [Phase 2 administration](docs/phase2-admin.md).

**Deduplication and CSV export.** Imports automatically skip identical stored sightings within a file
and across earlier imports. Results and import history report **Duplicates skipped**, including files
that contain only duplicates. Failed or cancelled files can be retried safely.

For duplicates stored by older versions, **Admin → Maintenance → Remove duplicate sightings** still
provides preview, cleanup and recovery. It preserves the earliest stored row, its ID, import history
and comparisons.

Use **Admin → Survey & backups → Export survey CSV** for one WiGLE CSV or a new folder containing
one file per day. Exports stream the active survey's stored rows; undated rows use `wigle_undated.csv`.
Authentication, altitude and other fields not retained by the survey are blank. Export does not
remove duplicates automatically. Existing Drive database files can be added through **Import scans**,
with duplicate sightings skipped automatically; the original archive file is left unchanged.

**Fits are kept across imports.** Adding a drive drops only the saved fits of networks it has new
sightings of (named networks by SSID, hidden ones by MAC); every other fit stays. **Fit every AP**
then refits just those. Graphite's Python drops every fit on every import; the app keeps the rest
because they can't have changed.

**Fast fits.** The explorer's **Fast fits** switch uses a quicker solver. It scores the same coarse
grid as the exact search and starts from the same eight best cells, but walks downhill from each
instead of scoring every 0.25 m point, then checks the 0.25 m points around where it lands. On a
test of 300 noisy networks it landed on exactly the same point 292 times and within a metre 298
times, about 7 times faster per network. Fast and exact fits are cached apart, and the switch is
off by default; the exact search is Graphite's method, checked against his Python.

**Background AP locations.** Surveys now prepare and save locations automatically, across the whole
survey, so they are available before you pan. The map shows progress and has a **Pause background**
button. In **Admin → Survey & backups → Prepare AP locations**, choose sample or all observations,
shared APs or individual radios, and background resource limits. The reference solver stays the
default; the app's fast-solver switch also applies to preparation. Completed and unavailable results
survive pauses, snapshots and restarts. Imports, cleanup and band edits queue only affected networks;
imports and selected calculations take priority over background work. Turning automatic preparation
off keeps calculated locations and restores manual fitting. Preparation now uses a continuous
worker pool and batched saves; Admin supports up to 16 workers while preserving
existing limits. See the [preparation benchmarks](docs/preparation-performance.md)
and [Phase 3 implementation](docs/phase3-preparation.md).

**Optional GPU fitting.** Admin's Calculation preference adds experimental
64-bit GPU acceleration on compatible Windows PCs, with automatic CPU fallback.
It works with reference or fast fitting and keeps their saved results separate.
Reference stays the default. Phase 5 also reduces CPU allocation/work and keeps
APs visible while replacement map pages load. See the
[solver comparison and full preparation results](docs/phase5-fitting.md).

Administration also opens a **read-only SQL browser** for the active survey.
It queries the real observation columns and a matching metadata snapshot, with
schema browsing, Explain, cancellation, timeouts, bounded pages and CSV export
of the displayed page. The dialect is SQLite. See the
[Phase 4 storage evaluation](docs/phase4-storage.md) for benchmarks, the storage
choice and the SQL adapter's limits.


**GPU map.** **GPU map** (on the home page and at the top of the explorer) draws every sighting in
view and every fitted AP with deck.gl over MapLibre, coloured by signal. Past the chosen number of
points (half a million to eight million) it shows every k-th sighting so the whole view is covered;
zoom in for all of them. It filters by band, signal and SSID, and hovering a fitted AP shows what it
is. It needs WebGL, which every supported system's web view has.

**Fit every AP.** The explorer normally fits access points near the map view as you pan. **Fit every
AP** fits every network the current filters keep, as one background job. It works through them a
batch of 256 networks at a time, saves each fit as it finishes and shows them on the map as it goes,
keeping the calculation queue bounded. **Cancel job** keeps what is done, and running it again
skips networks that already have a fit. A zoomed-out map shows an even sample of up to 1,500 fits;
zoom in to see every fit in an area.

**The map** opens where you are when the system will say. That means Windows with location
services on, and macOS after you allow it. Otherwise it opens where you last left it, or on the
whole world. Nothing is sent anywhere to find you. The explorer's store, exports and the map tile
cache live in the app's data folder:

| System | Data folder |
|---|---|
| Windows | `%APPDATA%\Signal Atlas\data` |
| macOS | `~/Library/Application Support/Signal Atlas/data` |
| Linux | `~/.config/Signal Atlas/data` (or under `$XDG_CONFIG_HOME`) |

**Updates.** The home page asks once whether Signal Atlas may check GitHub for new releases. If you
say yes, it asks once a day; **Check for updates** asks at any time. The request carries only the
app's version. When a newer release is out, the installed Windows app can install it: it downloads
the release's installer, checks it against the release's `SHA256SUMS.txt`, closes, installs quietly
and opens the new version. Elsewhere it opens the release page. Your data is never touched by an
update.

**Offline.** Reading, analysis and storage work offline. The GPU map bundles pinned
MapLibre, deck.gl and Leaflet assets, so it can initialize and draw survey data without a
library CDN. New street tiles still require OpenStreetMap access; cached tiles remain
available. Other pages may use online assets/fonts and fall back to their system fonts
or track outlines. Map tiles are cached for a week, and the app identifies itself to
OpenStreetMap as their tile policy asks.

## Large-survey improvements

The GPU map adds exact clustering, a clustering toggle, label zoom controls and solid
planning rings. Radius defaults to 10 m and is adjustable from 5–50 m. Admin provides
survey switching/creation, backups, recoverable 0,0 cleanup and a read-only SQL browser.
Preparation prioritizes the current view while continuing the whole survey in the
background. Hidden APs use their BSSID/MAC identity, not a shared hidden-name label.

On the documented 5.75M-scan / 1.46M-target survey, the metadata follow-up reduced
complete GPU-fast preparation from 336 seconds to 149–151 seconds, with identical
saved results. This comparison is against the pre-metadata implementation, not an
upstream speedup claim. Final engine peaks were about 3.14 GiB; observation columns
remain resident and DuckDB remains an isolated benchmark prototype. First opening an
existing large survey can take longer once while internal metadata is migrated.

See the [review and validation guide](docs/review-guide.md) for delivered features,
benchmark comparators, migrations, experimental boundaries and reproduction. The
[observation-quality findings](docs/observation-quality.md) describe optional future
filters; they are not enabled as production defaults.

## Earlier upstream speed measurements

The following measurements predate this follow-up and use a different 16-core PC
with 64 GB of memory. They are historical context, not new benchmark results.

| Pipeline | Rate |
|---|---|
| Explorer CSV import | 53–55 million rows a minute (50 million rows in 56 s) |
| Wardrive Compare, add and analyse | 76 million rows a minute |
| Explorer, opening a saved SQLite database | about 42 million rows a minute |
| Explorer, Fit every AP | about 13,000 networks a minute; 5 million rows (130,000 networks) in about 10 minutes, memory flat |
| Legacy Drive archive, adding logs | about 18 million rows a minute, the same at 10 million rows; under 0.2 GB of memory |
| Legacy Drive archive, adding a file already in it | about 50 million rows a minute (every row a duplicate) |
| Legacy Drive archive, exporting WiGLE CSV | about 58 million rows a minute, one file or one per day |
| Explorer queries | under 0.6 s at 10 million rows, under 3 s at 50 million |
| AP location fits | about 10,700 radios a minute |

Compare uses about 80 MB of memory per million rows while it works. The explorer's store holds about
150 MB per million observations while the app is open.

## How it works

The screens are web pages shown in a native window ([Wails](https://wails.io)). They talk to a Go
engine through the window's own origin, and nothing listens on the network.

| Path | What it is |
|---|---|
| `web/compare.html` | Wardrive Compare. It also runs on its own in a browser, using the analysis worker inside it. |
| `python/` | Graphite's Signal Atlas reference, with matching import/hidden-identity fixes: the Python package, its tests, and the explorer pages (`python/templates/`) that the app shows. |
| `internal/compare` | Compare's analysis in Go, ported line for line from the worker in `web/compare.html`. |
| `internal/atlas` | Signal Atlas in Go, ported from Graphite's Python: filters, queries, the location model and its grid search, fit groups, area estimates, comparisons. |
| `internal/drivedb` | Legacy archive support and readers for logs and databases. Compare and the explorer use its readers to take database files as the CSV logs they convert to. |
| `internal/obs` | The column store that holds the explorer's observations. |
| `internal/engine` | The local server: Signal Atlas's API, background jobs, compare sessions, native dialogs, saving exports, the map tile cache. |
| `internal/py`, `internal/pycsv` | Python's behaviour where results depend on it: `json.dumps` text for cache keys and group hashes, `float()` and `int()`, the `csv` module's record splitting. |
| `ui/` | The home page, database administration, the GPU map, and the bridge script loaded into each page. |
| `internal/engine/settings.go` | The app's settings, and the update check and install. |
| `packaging/`, `scripts/release.py` | The app icon, the Windows manifest, the Linux build container and desktop entry, the macOS app bundle, and the script that builds releases. |

Graphite's Python keeps observations in SQLite with an R-tree and six indexes, which spends minutes
building indexes at tens of millions of rows. The app keeps them in a column store instead, and
everything else in SQLite in his schema. **Save SQLite database** writes a complete file in his
format, which his Python opens as its own.

## Proof that the ports give the same answers

`go test ./...` runs two comparisons.

- **Signal Atlas.** A deliberately awkward synthetic survey goes through Graphite's Python and the Go
  port. The test compares every result: imports, every query under twenty filter sets (errors
  included), group keys, every fit in four modes, area plans, comparisons and the single-network
  report. Positions agree to a centimetre, and everything else to one part in a billion.
- **Wardrive Compare.** The page's own worker runs in Node next to the Go port, on the same drives
  under seven configurations. About 31,000 result values and the CSV exports match exactly.

The comparisons need Python 3.10+ with numpy, and Node. They are skipped when either is missing.
Graphite's own tests run with `python -m unittest discover -s tests` from `python/`.
For the complete portable PR checks, run `python scripts/check.py`; it requires NumPy
and Node so the numerical/worker comparisons cannot silently skip. Optional `--race`,
`--gpu` and `--cross` checks are documented in the [review guide](docs/review-guide.md).

## Building

Go 1.27 or newer. Windows needs no C compiler. macOS needs the Xcode command line tools
(`xcode-select --install`). Linux needs a C compiler, `pkg-config` and the GTK 3 and WebKitGTK 4.1
development packages (`libgtk-3-dev libwebkit2gtk-4.1-dev` on Debian and Ubuntu).

```bash
# Windows
go build -tags desktop,production -ldflags "-H windowsgui -X main.Version=dev" -o build/signal-atlas.exe .
# macOS
go build -tags desktop,production -ldflags "-X main.Version=dev" -o build/signal-atlas .
# Linux
go build -tags desktop,production,webkit2_41 -ldflags "-X main.Version=dev" -o build/signal-atlas .
```

`signal-atlas -serve 127.0.0.1:8790` serves the same engine to a browser instead of opening the
window, which is useful for testing; there are no file dialogs in that mode. `-data DIR` moves the
data folder.

To make a test survey and time the pipelines:

```bash
go run ./cmd/gensurvey -rows 10000000 -out big.csv
go run ./cmd/bench -csv big.csv
go run ./cmd/benchcompare big.csv
```

## Releasing

Releases are built on a maintainer's computer; no build runs on GitHub's servers.

```bash
python scripts/release.py
```

It writes the release files and `SHA256SUMS.txt` to `dist/<version>/`, taking the version from the
`VERSION` file.

- **Windows and Linux** are built in a Docker container (`packaging/linux/Dockerfile`, Ubuntu 22.04),
  so any computer with Docker makes the same files: the Windows installer (NSIS,
  `packaging/windows/installer.nsi`) and zip, the Linux tar.gz and the .deb.
- **macOS** builds only on a Mac. Run the same command there to add the universal app.

`--only windows,linux,macos` builds a subset. `--upload` publishes the files as the GitHub release
`v<version>` with the GitHub CLI, and adds to it if it already exists, so a Mac can add the macOS app
to a release made on Windows. The app icon is drawn by `packaging/icon/make_icon.py`.

**Code signing** is optional and happens on the maintainer's computer, with their own certificate:

- **Windows.** Set `SIGNAL_ATLAS_SIGN_COMMAND` to a command that signs one file, with `{file}` where
  its path goes. The script signs `signal-atlas.exe` before it is zipped and packed into the
  installer, then signs the installer. Code-signing certificates now live on a hardware token or a
  cloud service, so the command is usually `signtool` with the token, or with Azure Trusted Signing:

  ```bash
  SIGNAL_ATLAS_SIGN_COMMAND='signtool sign /fd sha256 /tr http://timestamp.digicert.com /td sha256 /a {file}' python scripts/release.py
  ```

- **macOS** (on the Mac). `SIGNAL_ATLAS_MAC_IDENTITY="Developer ID Application: Name (TEAMID)"` signs
  the app for distribution, and `SIGNAL_ATLAS_MAC_NOTARY_PROFILE` (a profile saved once with
  `xcrun notarytool store-credentials`) also has Apple notarize it and staples the ticket.

The releases are for 64-bit Intel and AMD PCs on Windows and Linux, and for every Mac. Windows on Arm
runs the Windows build through its built-in emulation.

## Credits

- **Wardrive Compare** is **[Champ](https://github.com/TheChampCodes)**'s work. `web/compare.html` is the tool, and
  `internal/compare` is its analysis ported to Go.
- **Signal Atlas**, its location method and its explorer are **[Graphite](https://github.com/GraphiteCA)**'s work. `python/` is
  his code as delivered, and `internal/atlas` is a Go port of it.

Third-party software and data are listed in [NOTICE.md](NOTICE.md).

## License

Signal Atlas is released under the [MIT License](LICENSE), including Champ's and Graphite's
code, which they gave to the project.
