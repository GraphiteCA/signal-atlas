# Notices

Signal Atlas is under the MIT License ([LICENSE](LICENSE)). The software and data below keep
their own licenses.

## Wardrive Compare, by Champ

Wardrive Compare is the work of [Champ](https://github.com/TheChampCodes). `web/compare.html` is the tool;
`internal/compare` is a Go port of its analysis.

## Signal Atlas, by Graphite

`python/` is [Graphite](https://github.com/GraphiteCA)'s Signal Atlas: the location method, the Python
implementation, its tests and the explorer pages the app shows. `internal/atlas` is a Go port of it.

## Software the app is built with

| Project | Used for | License |
|---|---|---|
| [Wails](https://github.com/wailsapp/wails) | the window | MIT |
| [go-webview2](https://github.com/wailsapp/go-webview2) | Microsoft WebView2 from Go | MIT |
| [zombiezen.com/go/sqlite](https://github.com/zombiezen/go-sqlite) | SQLite access | ISC |
| [modernc.org/sqlite](https://gitlab.com/cznic/sqlite) | SQLite in pure Go | BSD-3-Clause |
| [SQLite](https://sqlite.org) | the explorer's metadata store | public domain |
| [Leaflet](https://leafletjs.com) | maps (loaded from unpkg.com) | BSD-2-Clause |

The full dependency list is in `go.mod`; each module carries its own license.

## Data

Map tiles and the explorer's offline basemap excerpts come from
[OpenStreetMap](https://www.openstreetmap.org/copyright), © OpenStreetMap contributors, under the
Open Database License (ODbL) 1.0. Optional satellite imagery in the explorer comes from Esri, under
the user's own ArcGIS key and Esri's terms.
