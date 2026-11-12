# Notices

Signal Atlas is under the MIT License ([LICENSE](LICENSE)). The software and data below keep
their own licenses.

## Wardrive Compare, by Champ

Wardrive Compare is the work of [Champ](https://github.com/TheChampCodes). `web/compare.html` is the tool;
`internal/compare` is a Go port of its analysis.

## Signal Atlas, by Graphite

`internal/atlas` is a Go port of [Graphite](https://github.com/GraphiteCA)'s Signal Atlas
location method and survey engine. The original Python implementation and explorer UI have been removed.

## Software the app is built with

| Project | Used for | License |
|---|---|---|
| [Wails](https://github.com/wailsapp/wails) | the window | MIT |
| [go-webview2](https://github.com/wailsapp/go-webview2) | Microsoft WebView2 from Go | MIT |
| [zombiezen.com/go/sqlite](https://github.com/zombiezen/go-sqlite) | SQLite access | ISC |
| [modernc.org/sqlite](https://gitlab.com/cznic/sqlite) | SQLite in pure Go | BSD-3-Clause |
| [SQLite](https://sqlite.org) | the survey metadata store | public domain |
| [Leaflet](https://leafletjs.com) | maps (GPU-map copy bundled under `ui/vendor/leaflet`) | BSD-2-Clause |
| [MapLibre GL JS](https://maplibre.org) | the GPU map's base map (bundled under `ui/vendor/maplibre`) | BSD-3-Clause |
| [deck.gl](https://deck.gl) | the GPU map's points (bundled under `ui/vendor/deck`) | MIT |
| [NSIS](https://nsis.sourceforge.io) | the Windows installer | zlib/libpng |

The full dependency list is in `go.mod`; each module carries its own license.

## Data

Map tiles and the explorer's offline basemap excerpts come from
[OpenStreetMap](https://www.openstreetmap.org/copyright), © OpenStreetMap contributors, under the
Open Database License (ODbL) 1.0. Optional satellite imagery in the explorer comes from Esri, under
the user's own ArcGIS key and Esri's terms.
