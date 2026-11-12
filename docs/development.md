# Development and releases

## Architecture

The screens are web pages shown in a native window ([Wails](https://wails.io)). They talk to a Go
engine through the window's own origin. Normal desktop mode does not open a TCP
listener; the explicit `-serve` development mode does.

| Path | What it is |
|---|---|
| `web/compare.html` | Wardrive Compare. It also runs on its own in a browser, using the analysis worker inside it. |
| `internal/compare` | Compare's analysis in Go, ported line for line from the worker in `web/compare.html`. Its device counts and list (`devices.go`) run only in the app. |
| `internal/devices` | Device identification: manufacturer and kind from a MAC, a name and a Bluetooth manufacturer id, with the tables it reads in `data/`. |
| `internal/atlas` | Survey analysis in Go: filters, queries, the location model and its grid search, fit groups, area estimates, comparisons. |
| `internal/drivedb` | Legacy archive support and readers for logs and databases. Compare and survey imports use its readers to take database files as the CSV logs they convert to. |
| `internal/obs` | The column store that holds the survey's observations. |
| `internal/engine` | The local server: Signal Atlas's API, background jobs, compare sessions, native dialogs, saving exports, the map tile cache. |
| `internal/py`, `internal/pycsv` | Python's behaviour where results depend on it: `json.dumps` text for cache keys and group hashes, `float()` and `int()`, the `csv` module's record splitting. |
| `ui/` | The home page, database administration, the GPU map, and the bridge script loaded into each page. |
| `internal/engine/settings.go` | The app's settings, and the update check and install. |
| `packaging/`, `scripts/release.py` | The app icon, the Windows manifest, the Linux build container and desktop entry, the macOS app bundle, and the script that builds releases. |

The original survey analysis was written in Python by Graphite. The shipping
application uses Go; the original Python implementation has been removed.
`internal/py` and `internal/pycsv` are Go compatibility helpers, not a Python runtime.
Recorded reference fixtures in `test/parity/` retain regression coverage of the
original implementation (see `internal/parity/parity_test.go` for their source revision).

Native workspaces store observations in immutable columns and metadata in SQLite,
including preservation tables. **Save snapshot → Portable SQLite** materializes
observations and metadata into a standalone database. See the [schema](schema.md)
and [preservation guide](import-export.md) for compatibility and storage contracts.

## Regression and parity checks

`go test ./...` runs the Go suite, including two reference comparisons:

- **Signal Atlas.** A deliberately awkward synthetic survey runs through Go against recorded reference results. The test compares covered results: imports, every query under twenty filter sets (errors
  included), group keys, every fit in four modes, area plans, comparisons and the single-network
  report. Positions agree to a centimetre, and everything else to one part in a billion.
- **Wardrive Compare.** The page's own worker runs in Node next to the Go port, on the same drives
  under seven configurations. About 31,000 result values and the CSV exports match exactly.

The Signal Atlas regression test runs entirely in Go. Compare tests use Python for drive fixtures
and Node for the active JavaScript worker. For complete portable PR checks, run
`python scripts/check.py`; it requires Go, Python and Node. Optional `--race`,
`--gpu` and `--cross` checks are documented in the [review guide](review-guide.md).

## Building

Go 1.27.3 or newer (the version declared in `go.mod`). Windows needs no C compiler. macOS needs the Xcode command line tools
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
