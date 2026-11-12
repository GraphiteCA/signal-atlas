# Survey schema reference

The current logical SQLite schema version is **6** (`PRAGMA user_version`). Native
workspace format version remains **1**. The formats version different contracts:
SQLite metadata/portable tables versus the workspace generation pointer.

## Observation fields

These fields appear in portable SQLite `observations` and the SQL browser's
read-only observation adapter. Native observations live in column segments;
the internal metadata database's compatibility table is intentionally empty.

| Field | Type / meaning |
|---|---|
| `id` | INTEGER primary key; stable stored observation ID, not a source row ID |
| `import_id` | INTEGER; import that first supplied this stored observation |
| `line` | INTEGER; source CSV record sequence or adapter sequence; multiline fields mean this is not necessarily a physical line number |
| `record_id` | TEXT; source `id`, or the record sequence when absent |
| `ssid` | TEXT; network name, possibly empty for hidden networks |
| `mac` | TEXT; trimmed uppercase MAC/BSSID; parser requires nonempty text |
| `band` | TEXT; `2.4`, `5`, `6`, `other`, or `unknown` |
| `band_source` | TEXT; frequency, explicit band, unavailable/conflicting evidence, or manual assignment |
| `radio_key` | TEXT; compact JSON array `[ssid,mac,band]` |
| `latitude` | REAL; degrees north |
| `longitude` | REAL; degrees east |
| `rssi_dbm` | REAL; received signal in dBm |
| `accuracy_m` | REAL or NULL; nonnegative GPS accuracy in metres |
| `first_seen` | TEXT; timestamp as imported/converted, empty if unavailable |
| `date` | TEXT; first ten timestamp characters; not a timezone conversion |
| `channel` | TEXT; source channel or channel calculated by a database adapter |
| `frequency_mhz` | REAL or NULL; normalized positive frequency in MHz |

CSV and WiGLE scan parsing require finite coordinates/RSSI, `-85 < latitude < 85`,
`-180 <= longitude <= 180`, and `-150 <= RSSI <= 0`. Provided accuracy must be
finite and nonnegative. Numeric `0,0` pairs are excluded before measurement
validation. Portable-survey conversion accepts geographic latitude bounds up to
90 degrees and preserves saved measurements/IDs, while excluding zero pairs.
Missing numeric accuracy/frequency is NULL in SQLite (an internal NaN sentinel
in the column store). Missing text is generally the empty string.

## Preservation tables

| Table | Columns and contract |
|---|---|
| `source_files` | `sha256 TEXT PRIMARY KEY`, `size_bytes INTEGER`; one content-addressed original file |
| `source_chunks` | `(sha256,chunk)` primary key, `payload BLOB`; zero-based ordered chunks of at most 1 MiB, concatenated to recover the original |
| `import_sources` | `import_id INTEGER PRIMARY KEY`, `sha256 TEXT`, `filename TEXT`; links an import to its archived original |
| `observation_details` | `observation_id INTEGER PRIMARY KEY`, `fields_json TEXT`; JSON object of additional field names to nonempty exact text values |
| `observation_provenance` | `(import_id,line)` primary key, `record_id TEXT`, nullable `observation_id INTEGER`, `source_table TEXT`, `source_record_source TEXT`; every accepted source row, including duplicates |

Wardrive Compare reads `observation_provenance` to put exact duplicates back when it takes
drives from the survey: an observation is written once for each source row recorded for it,
in the drive of the import that supplied that row. A stored row with no record of its own
(surveys older than schema 6) still counts once.

Observation references are logical references to the column store, not foreign
keys to the empty metadata observation table. Cleanup clears a provenance link
when its observation is removed; duplicate cleanup remaps it to the survivor.
Extra fields participate in exact deduplication. Source-local IDs, table/source
labels and filenames do not. Original bytes retain distinctions intentionally
normalized by parsing. CSV fields have a 131,072-character parser limit; an
over-limit field fails the file rather than silently truncating it. SHA-256 and byte length are checked on original export.

Sources and details remain on disk; source archival/extraction uses bounded
chunks, and provenance staging uses a temporary SQLite transaction. Importing
very large individual field values still requires memory for those values.

## Metadata and identities

- `imports`: filename, content hash, import UTC time, original accepted/invalid/
  non-Wi-Fi/zero-excluded/duplicate counts, active row count and later zero-removal
  count. `valid_rows` counts newly stored observations, not provenance rows.
- `estimates`: fitted-result JSON keyed by radio/group and scope; calculated data,
  not original measurements.
- `comparisons`: saved ID, name, creation time and comparison JSON payload.
- `compare_runs`: Wardrive Compare results saved from the Compare page: `id`, `name`,
  `created_utc`, `drives` (count), `sources_json` (the drives compared), `state_json` (page
  settings, drive names and notes) and `result_json` (the analysis as computed). A run is a
  snapshot and is never recomputed. The table is additive and optional, so it does not change
  the schema version: older applications open the survey and leave the table alone, but a
  snapshot they convert will not carry it.
- `band_assignments`: manual band by SSID/MAC, with update time.
- `map_assets`: stored basemap and survey preferences. `map_revision` and
  `map_changes` support incremental map refresh.
- `preparation_*`: derived catalogues, target versions, work queues, solver
  namespaces and checkpoints used by background location preparation.
- Portable SQLite includes the `spatial` R-tree and lookup indexes/catalogue
  views. Native map/SQL reads use indexes over the actual observation columns.

Full survey conversion preserves stored observation IDs. Import-as-scans creates
destination IDs and keeps source record identifiers as provenance. A named radio
is SSID/MAC/band; hidden AP grouping uses MAC identity rather than grouping every
empty SSID together. Saved fit identity also depends on grouping, scope and solver.

## Compatibility and migration

Schema 6 adds preservation tables and supports fractional WiGLE timestamps.
Opening older workspaces creates empty preservation tables; no original content
or duplicate provenance is fabricated. Applications rejecting versions above
their supported schema will require an update to open new portable snapshots.
The 17-column observation schema stays compatible for independent SQL readers.

Native binary segments are an internal storage format, not an interchange API.
Use portable SQLite, documented CSV exports, or archived originals. Unknown
vendor data has no implied units beyond its source's definition; retention does
not imply Atlas interprets or validates it.

See [import/export behavior](import-export.md) for fidelity guarantees, user
workflows, exclusions and the difference between CSV interchange and a backup.
