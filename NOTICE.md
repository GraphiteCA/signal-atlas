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

## Device identification data

`internal/devices/data` holds the tables Compare's device identification reads:

- `oui.tsv.gz`: MAC prefix to organization, from the IEEE's public MA-L, MA-M and MA-S registries
  ([standards-oui.ieee.org](https://standards-oui.ieee.org)).
- `ble_companies.tsv.gz`: Bluetooth company identifiers, from the Bluetooth SIG's public assigned
  numbers ([bitbucket.org/bluetooth-SIG/public](https://bitbucket.org/bluetooth-SIG/public)).
- `ble_categories.tsv`: company identifier to device category, from the company-category tables
  in the reverse-engineering report of Warble 3.1.321 (Nov 9, 2026, @jbohack).
- `notable_prefixes.tsv`: the hardcoded MAC prefixes of Warble 3.1.321's detection tables, as the
  reverse-engineering report of Nov 9, 2026 (@jbohack) lists them.
- `adult_toy_names.txt`: adult toys' Bluetooth names from the
  [buttplug.io](https://github.com/buttplugio/buttplug) device configuration (BSD-3-Clause), as that
  report lists them.

The name rules and Bluetooth company rules in `internal/devices/devices.go` follow the same
report; the everyday categories follow WarHog's Insights.

## Data

Map tiles and the explorer's offline basemap excerpts come from
[OpenStreetMap](https://www.openstreetmap.org/copyright), © OpenStreetMap contributors, under the
Open Database License (ODbL) 1.0. Optional satellite imagery in the explorer comes from Esri, under
the user's own ArcGIS key and Esri's terms.
