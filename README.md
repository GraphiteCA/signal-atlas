# Signal Atlas

A desktop app for Windows, macOS and Linux, for wardriving data too big for the browser. It has two
tools:

- **Wardrive Compare**, by **[Champ](https://github.com/TheChampCodes)**. Drop WiGLE CSV logs from any logger and compare drives on the road they
  share: networks per second and per km, how long networks stay visible, speed bands, channels, a
  baseline drive with a bootstrap range and fairness checks, repeat passes, multi-node rigs.
- **Signal Atlas explorer**, by **[Graphite](https://github.com/GraphiteCA)**. Import whole CSV surveys, filter by SSID, MAC, band,
  date and signal, and fit where each network's access point is. It supports one fit per radio or
  one shared location per SSID, nearby fits as you pan, and pinned date-range comparisons.

It runs in its own window and does not open a browser. Your data never leaves the computer, and the
app is dark-themed throughout.

## Running it

Download your system's file from the Releases page.

| System | File | Notes |
|---|---|---|
| Windows 10 or 11 | `signal-atlas-<version>-windows-amd64.zip` | Unzip and run `signal-atlas.exe`. The screens are drawn by WebView2, which Windows already includes. |
| macOS 12 or newer | `signal-atlas-<version>-macos-universal.zip` | Unzip and move **Signal Atlas** to Applications. One app runs on Apple silicon and Intel Macs. |
| Ubuntu 22.04+, Debian 12+ | `signal-atlas_<version>_amd64.deb` | `sudo apt install ./signal-atlas_<version>_amd64.deb` installs it and what it needs. |
| Other Linux | `signal-atlas-<version>-linux-amd64.tar.gz` | Needs GTK 3 and WebKitGTK 4.1 (`libwebkit2gtk-4.1-0` on Debian and Ubuntu, `webkit2gtk4.1` on Fedora). |

The builds are not code-signed, so each system warns the first time:

- **Windows.** SmartScreen may say the app is unrecognised. Choose **More info**, then **Run anyway**.
- **macOS.** The first open is refused. Open **System Settings → Privacy & Security** and choose
  **Open Anyway** next to Signal Atlas, or right-click the app and choose **Open**.

Files are read where they are on disk. Drop logs on the window, or use **Choose files** and
**Import CSV**, which open the system's file dialog. Exports and saved databases go through the
system's Save dialog. The explorer's store, exports and the map tile
cache live in the app's data folder:

| System | Data folder |
|---|---|
| Windows | `%APPDATA%\Signal Atlas\data` |
| macOS | `~/Library/Application Support/Signal Atlas/data` |
| Linux | `~/.config/Signal Atlas/data` (or under `$XDG_CONFIG_HOME`) |

**Offline.** Everything that reads, analyses and stores data works offline. The map library
(Leaflet) loads from unpkg.com, map tiles come from OpenStreetMap, and fonts come from Google Fonts.
Without a connection, Compare draws its map as a plain outline of the tracks, the explorer's map
doesn't load, and the pages use the system's own fonts. Map tiles are cached for a week, and the app
identifies itself to OpenStreetMap as their tile policy asks.

## Speed

Measured on a 16-core PC with 64 GB of memory.

| Pipeline | Rate |
|---|---|
| Explorer CSV import | 53–55 million rows a minute (50 million rows in 56 s) |
| Wardrive Compare, add and analyse | 76 million rows a minute |
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
| `python/` | Graphite's Signal Atlas as he wrote it: the Python package, its tests, and the explorer pages (`python/templates/`) that the app shows. |
| `internal/compare` | Compare's analysis in Go, ported line for line from the worker in `web/compare.html`. |
| `internal/atlas` | Signal Atlas in Go, ported from Graphite's Python: filters, queries, the location model and its grid search, fit groups, area estimates, comparisons. |
| `internal/obs` | The column store that holds the explorer's observations. |
| `internal/engine` | The local server: Signal Atlas's API, background jobs, compare sessions, native dialogs, saving exports, the map tile cache. |
| `internal/py`, `internal/pycsv` | Python's behaviour where results depend on it: `json.dumps` text for cache keys and group hashes, `float()` and `int()`, the `csv` module's record splitting. |
| `ui/` | The home page and the bridge script loaded into each page. |
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
  so any computer with Docker makes the same files: the Windows zip, the Linux tar.gz and the .deb.
- **macOS** builds only on a Mac. Run the same command there to add the universal app.

`--only windows,linux,macos` builds a subset. `--upload` publishes the files as the GitHub release
`v<version>` with the GitHub CLI, and adds to it if it already exists, so a Mac can add the macOS app
to a release made on Windows. The app icon is drawn by `packaging/icon/make_icon.py`.

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
