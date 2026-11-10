"""Streaming CSV normalization and indexed storage for large, multi-network surveys."""
from __future__ import annotations

import csv
import hashlib
import json
import math
from pathlib import Path
import re
import sqlite3
from contextlib import contextmanager
from contextlib import closing
from datetime import date, datetime, timezone

REQUIRED_COLUMNS = {"MAC", "SSID", "RSSI", "CurrentLatitude", "CurrentLongitude"}
BANDS = ("2.4", "5", "6", "other", "unknown")
SCHEMA_VERSION = 5


def discover_csv_files(sources):
    """Find all surveys, including nested folders and uppercase CSV extensions."""
    found = {}
    for source in sources:
        source = Path(source)
        paths = (p for p in source.rglob('*') if p.is_file() and p.suffix.lower()=='.csv') if source.is_dir() else [source]
        for path in paths:
            found[path.resolve()] = path
    return sorted(found.values(), key=lambda path: str(path).casefold())


def csv_reader(handle):
    """Accept ordinary CSV or WiGLE's one-line metadata preamble."""
    source = csv.reader(handle)
    header = next(source, [])
    first_data_line = 2
    if header and header[0].strip().lower().startswith("wiglewifi-"):
        header = next(source, [])
        first_data_line = 3
    header = [name.strip() for name in header]
    missing = REQUIRED_COLUMNS - set(header)
    if missing:
        raise ValueError("Missing CSV columns: " + ", ".join(sorted(missing)))
    return csv.DictReader(handle, fieldnames=header), first_data_line


def is_wifi(row):
    return (row.get("Type") or "WIFI").strip().upper() == "WIFI"


def frequency_mhz(value):
    text = str(value or "").strip().lower().replace(" ", "")
    if not text:
        return None
    match = re.fullmatch(r"([\d.]+)(ghz|mhz|khz|hz)?", text)
    if not match:
        return None
    try:
        number = float(match[1])
    except ValueError:
        return None
    if not math.isfinite(number) or number <= 0:
        return None
    unit = match[2]
    factor = {"ghz": 1000, "mhz": 1, "khz": .001, "hz": .000001}.get(unit)
    if factor is None:
        factor = .000001 if number >= 1e8 else .001 if number >= 1e6 else 1000 if number < 100 else 1
    return number * factor


def infer_band(frequency, explicit=None):
    mhz = frequency_mhz(frequency)
    measured = None
    if mhz is not None:
        measured = "2.4" if 2400 <= mhz < 2500 else "5" if 4900 <= mhz < 5925 else "6" if 5925 <= mhz <= 7125 else "other"
    declared = str(explicit or "").lower().replace("ghz", "").replace(" ", "")
    declared = declared if declared in ("2.4", "5", "6") else None
    if measured and declared and measured != declared:
        return "unknown", "conflicting frequency and explicit band", mhz
    if measured:
        return measured, "frequency", mhz
    if declared:
        return declared, "explicit band", mhz
    # Channel numbers overlap across bands, particularly with 6 GHz. Never guess
    # a band from the channel, the network name, or adjacent MAC addresses.
    return "unknown", "frequency/band unavailable", mhz


def parse_observation(row, line):
    try:
        lat, lon, rssi = (float(row[k]) for k in ("CurrentLatitude", "CurrentLongitude", "RSSI"))
        accuracy = float(row["AccuracyMeters"]) if row.get("AccuracyMeters") else None
    except (ValueError, TypeError, KeyError) as exc:
        raise ValueError(f"Invalid numeric measurement on CSV line {line}") from exc
    if not all(math.isfinite(v) for v in (lat, lon, rssi)):
        raise ValueError(f"Non-finite measurement on CSV line {line}")
    if not (-85 < lat < 85 and -180 <= lon <= 180 and -150 <= rssi <= 0):
        raise ValueError(f"Out-of-range coordinates/RSSI on CSV line {line}")
    if accuracy is not None and (not math.isfinite(accuracy) or accuracy < 0):
        raise ValueError(f"Invalid GPS accuracy on CSV line {line}")
    mac = (row.get("MAC") or "").strip().upper()
    if not mac:
        raise ValueError(f"Missing MAC on CSV line {line}")
    ssid = row.get("SSID") or ""
    band, band_source, mhz = infer_band(row.get("Frequency"), row.get("Band") or row.get("band"))
    return {
        "key": str(line), "id": row.get("id") or str(line), "ssid": ssid, "mac": mac,
        "latitude": lat, "longitude": lon, "rssi_dbm": rssi, "accuracy_m": accuracy,
        "first_seen": row.get("FirstSeen") or "", "day_table": row.get("day_table") or "",
        "date": (row.get("FirstSeen") or "")[:10], "channel": row.get("Channel") or "",
        "frequency": row.get("Frequency") or "", "frequency_mhz": mhz,
        "band": band, "band_source": band_source, "source": row.get("source") or "",
        "radio_key": json.dumps([ssid, mac, band], separators=(",", ":"), ensure_ascii=False),
    }


@contextmanager
def connect(database):
    connection = sqlite3.connect(str(database), timeout=30)
    connection.row_factory = sqlite3.Row
    connection.execute("PRAGMA foreign_keys=ON")
    try:
        with connection:
            yield connection
    finally:
        connection.close()


def initialize(database):
    Path(database).parent.mkdir(parents=True, exist_ok=True)
    with connect(database) as db:
        if db.execute("PRAGMA user_version").fetchone()[0] > SCHEMA_VERSION:
            raise ValueError("This database requires a newer Signal Atlas version")
        tables = {row[0] for row in db.execute("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'")}
        if tables and not {"observations", "imports"}.issubset(tables):
            raise ValueError("This is not a Signal Atlas database; choose a new file or an existing Signal Atlas store")
        db.execute("PRAGMA journal_mode=WAL")
        db.executescript('''
          CREATE TABLE IF NOT EXISTS imports(id INTEGER PRIMARY KEY, filename TEXT NOT NULL,
            sha256 TEXT UNIQUE NOT NULL, imported_utc TEXT NOT NULL, valid_rows INTEGER NOT NULL DEFAULT 0,
            invalid_rows INTEGER NOT NULL DEFAULT 0);
          CREATE TABLE IF NOT EXISTS observations(
            id INTEGER PRIMARY KEY, import_id INTEGER NOT NULL REFERENCES imports(id), line INTEGER NOT NULL,
            record_id TEXT, ssid TEXT NOT NULL, mac TEXT NOT NULL, band TEXT NOT NULL, band_source TEXT,
            radio_key TEXT NOT NULL, latitude REAL NOT NULL, longitude REAL NOT NULL, rssi_dbm REAL NOT NULL,
            accuracy_m REAL, first_seen TEXT, date TEXT, channel TEXT, frequency_mhz REAL);
          CREATE VIRTUAL TABLE IF NOT EXISTS spatial USING rtree(id,min_lat,max_lat,min_lon,max_lon);
          CREATE TRIGGER IF NOT EXISTS observation_insert AFTER INSERT ON observations BEGIN
            INSERT INTO spatial VALUES(new.id,new.latitude,new.latitude,new.longitude,new.longitude); END;
          CREATE TRIGGER IF NOT EXISTS observation_delete AFTER DELETE ON observations BEGIN
            DELETE FROM spatial WHERE id=old.id; END;
          CREATE INDEX IF NOT EXISTS observation_network ON observations(ssid,band);
          CREATE INDEX IF NOT EXISTS observation_radio ON observations(radio_key,rssi_dbm DESC);
          CREATE INDEX IF NOT EXISTS observation_radio_date ON observations(radio_key,date);
          CREATE INDEX IF NOT EXISTS observation_date ON observations(date);
          CREATE INDEX IF NOT EXISTS observation_transmitter ON observations(ssid,mac,band);
          CREATE INDEX IF NOT EXISTS observation_mac ON observations(mac,ssid);
          CREATE TABLE IF NOT EXISTS estimates(radio_key TEXT NOT NULL,scope_key TEXT NOT NULL,
            result TEXT NOT NULL,PRIMARY KEY(radio_key,scope_key));
          CREATE TABLE IF NOT EXISTS map_assets(name TEXT PRIMARY KEY,payload TEXT NOT NULL);
          CREATE TABLE IF NOT EXISTS comparisons(id TEXT PRIMARY KEY,name TEXT NOT NULL,
            created_utc TEXT NOT NULL,payload TEXT NOT NULL);
          CREATE TABLE IF NOT EXISTS band_assignments(ssid TEXT NOT NULL,mac TEXT NOT NULL,
            band TEXT NOT NULL,updated_utc TEXT NOT NULL,PRIMARY KEY(ssid,mac));
          CREATE VIEW IF NOT EXISTS network_catalog AS SELECT ssid,count(*) count,
            count(DISTINCT mac) macs,count(DISTINCT radio_key) radios,max(rssi_dbm) peak,
            group_concat(DISTINCT band) bands FROM observations GROUP BY ssid;
          CREATE VIEW IF NOT EXISTS transmitter_catalog AS SELECT ssid,mac,count(*) count,
            count(DISTINCT band) radio_count,group_concat(DISTINCT band) bands,max(rssi_dbm) peak
            FROM observations GROUP BY ssid,mac;
          CREATE VIEW IF NOT EXISTS radio_catalog AS SELECT radio_key,ssid,mac,band,count(*) count,
            max(rssi_dbm) peak FROM observations GROUP BY radio_key;
        ''')
        if "non_wifi_rows" not in {r[1] for r in db.execute("PRAGMA table_info(imports)")}:
            db.execute("ALTER TABLE imports ADD COLUMN non_wifi_rows INTEGER NOT NULL DEFAULT 0")
        if "scope_key" not in {r[1] for r in db.execute("PRAGMA table_info(estimates)")}:
            if not db.in_transaction:
                db.execute("BEGIN IMMEDIATE")
            db.execute("ALTER TABLE estimates RENAME TO estimates_previous_schema")
            db.execute("CREATE TABLE estimates(radio_key TEXT NOT NULL,scope_key TEXT NOT NULL,result TEXT NOT NULL,PRIMARY KEY(radio_key,scope_key))")
            db.execute("INSERT INTO estimates SELECT radio_key,'legacy-full-history-sampled-v1',result FROM estimates_previous_schema")
            db.execute("DROP TABLE estimates_previous_schema")
        db.execute('CREATE INDEX IF NOT EXISTS estimate_scope_lookup ON estimates(scope_key)')
        db.execute("""CREATE INDEX IF NOT EXISTS estimate_viewport ON estimates(
            scope_key,json_extract(result,'$.longitude'),json_extract(result,'$.latitude'))
            WHERE json_extract(result,'$.status')='fitted'""")
        db.execute(f"PRAGMA user_version={SCHEMA_VERSION}")
        # Adopt the old sidecar once; the database is authoritative thereafter.
        sidecar = Path(database).with_name("basemap.geojson")
        if not db.execute("SELECT 1 FROM map_assets WHERE name='basemap'").fetchone() and sidecar.is_file():
            try:
                payload = read_basemap(sidecar)
            except (OSError, ValueError):
                payload = None  # Optional context must not prevent opening a survey.
            if payload is not None:
                db.execute("INSERT INTO map_assets VALUES('basemap',?)", (json.dumps(payload),))


def read_basemap(path):
    path = Path(path)
    if path.stat().st_size > 20_000_000:
        raise ValueError("Local map excerpt exceeds 20 MB")
    payload = json.loads(path.read_text(encoding="utf-8"))
    if not isinstance(payload, dict) or payload.get("type") != "FeatureCollection" or not isinstance(payload.get("features"), list):
        raise ValueError("Expected a GeoJSON FeatureCollection")
    return payload


def store_basemap(database, path):
    payload = read_basemap(path)
    initialize(database)
    with connect(database) as db:
        db.execute("INSERT OR REPLACE INTO map_assets VALUES('basemap',?)", (json.dumps(payload),))


def load_basemap(database):
    with connect(database) as db:
        row = db.execute("SELECT payload FROM map_assets WHERE name='basemap'").fetchone()
        return json.loads(row[0]) if row else None


def database_info(database):
    """Describe the canonical store without exposing a machine-specific path."""
    path = Path(database)
    with connect(path) as db:
        return {"filename": path.name, "schema_version": db.execute("PRAGMA user_version").fetchone()[0],
                "sqlite_version": sqlite3.sqlite_version,
                "observations": db.execute("SELECT count(*) FROM observations").fetchone()[0],
                "networks": db.execute("SELECT count(*) FROM (SELECT DISTINCT ssid FROM observations)").fetchone()[0],
                "transmitters": db.execute("SELECT count(*) FROM (SELECT DISTINCT ssid,mac FROM observations)").fetchone()[0],
                "cached_estimates": db.execute("SELECT count(*) FROM estimates").fetchone()[0],
                "has_basemap": bool(db.execute("SELECT 1 FROM map_assets WHERE name='basemap'").fetchone()),
                "imports": db.execute("SELECT count(*) FROM imports").fetchone()[0]}


def backup_database(database, destination):
    """Create a consistent standalone SQLite snapshot, including committed WAL data."""
    destination = Path(destination)
    if not Path(database).is_file():
        raise ValueError("Source database not found")
    if Path(database).resolve() == destination.resolve():
        raise ValueError("Choose a backup path different from the live database")
    destination.parent.mkdir(parents=True, exist_ok=True)
    # Exclusive reservation avoids overwriting an existing user database.
    with destination.open("xb"):
        pass
    try:
        with connect(database) as source, closing(sqlite3.connect(str(destination))) as target:
            source.backup(target)
        return destination
    except Exception:
        destination.unlink(missing_ok=True)
        raise


def import_csv(path, database, skip_invalid=False, progress=None, filename=None):
    """Stream rows in bounded batches; rollback the entire import on failure."""
    path = Path(path)
    initialize(database)
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for block in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(block)
    checksum = digest.hexdigest()
    columns = ("record_id", "ssid", "mac", "band", "band_source", "radio_key", "latitude", "longitude",
               "rssi_dbm", "accuracy_m", "first_seen", "date", "channel", "frequency_mhz")
    valid, invalid, non_wifi, examples, batch = 0, 0, 0, [], []
    with connect(database) as db:
        existing = db.execute("SELECT * FROM imports WHERE sha256=?", (checksum,)).fetchone()
        if existing:
            return {"duplicate": True, "valid_rows": existing["valid_rows"], "invalid_rows": existing["invalid_rows"], "non_wifi_rows": existing["non_wifi_rows"]}
        cursor = db.execute("INSERT INTO imports(filename,sha256,imported_utc) VALUES(?,?,?)",
                            (filename or path.name, checksum, datetime.now(timezone.utc).isoformat()))
        import_id = cursor.lastrowid
        assignments = {(r['ssid'],r['mac']):r['band'] for r in db.execute('SELECT * FROM band_assignments')}
        insert = f"INSERT INTO observations(import_id,line,{','.join(columns)}) VALUES({','.join('?' for _ in range(len(columns)+2))})"
        with path.open(encoding="utf-8-sig", newline="") as handle:
            reader, start_line = csv_reader(handle)
            for line, raw in enumerate(reader, start_line):
                if not is_wifi(raw):
                    non_wifi += 1
                    continue
                try:
                    row = parse_observation(raw, line)
                except ValueError as exc:
                    if not skip_invalid:
                        raise
                    invalid += 1
                    if len(examples) < 10:
                        examples.append(str(exc))
                    continue
                row["record_id"] = row["id"]
                assigned = assignments.get((row['ssid'],row['mac']))
                if assigned and row['band_source']=='frequency/band unavailable':
                    row.update(band=assigned,band_source='manual assignment',radio_key=json.dumps(
                        [row['ssid'],row['mac'],assigned],separators=(',',':'),ensure_ascii=False))
                batch.append((import_id, line, *(row[k] for k in columns)))
                valid += 1
                if len(batch) == 5000:
                    db.executemany(insert, batch)
                    batch.clear()
                    if progress:
                        progress(valid, invalid, handle.buffer.tell(), path.stat().st_size)
            if batch:
                db.executemany(insert, batch)
            if progress:
                progress(valid, invalid, path.stat().st_size, path.stat().st_size)
        if not valid:
            raise ValueError("CSV contains no valid observations")
        db.execute("UPDATE imports SET valid_rows=?,invalid_rows=?,non_wifi_rows=? WHERE id=?", (valid, invalid, non_wifi, import_id))
        # Appending data can change fitted locations; stale results are never reused.
        db.execute("DELETE FROM estimates")
    return {"duplicate": False, "valid_rows": valid, "invalid_rows": invalid, "non_wifi_rows": non_wifi, "invalid_examples": examples}


def filters(params, spatial=False):
    clauses, values = [], []
    if params.get('ssid_query', [''])[0]:
        needle = params['ssid_query'][0].replace('\\', '\\\\').replace('%', '\\%').replace('_', '\\_')
        clauses.append("o.ssid LIKE ? ESCAPE '\\'")
        values.append(f'%{needle}%')
    if params.get("none") == ["1"]:
        clauses.append("0")
    for key, column, exclude in (("ssid", "ssid", False), ("exclude_ssid", "ssid", True),
                                  ("mac", "mac", False),
                                  ("radio", "radio_key", False), ("exclude_radio", "radio_key", True),
                                  ("band", "band", False)):
        terms = params.get(key, [])
        if len(terms) > 200:
            raise ValueError("Select at most 200 individual filter values, or use All")
        if terms:
            if key == "mac":
                terms = [term.strip().upper() for term in terms]
            clauses.append(f"o.{column} {'NOT IN' if exclude else 'IN'} ({','.join('?' for _ in terms)})")
            values.extend(terms)
    if params.get("mac_selection"):
        choices = json.loads(params["mac_selection"][0])
        if not isinstance(choices, dict) or len(choices) > 100:
            raise ValueError("MAC selections must be scoped to at most 100 SSIDs")
        total = 0
        for ssid, choice in choices.items():
            if not isinstance(choice, dict) or choice.get("mode") not in ("include", "exclude"):
                raise ValueError("Invalid SSID/MAC selection mode")
            macs = choice.get("macs")
            if not isinstance(macs, list) or not all(isinstance(mac, str) for mac in macs):
                raise ValueError("MAC selections must contain a list of MAC strings")
            total += len(macs)
            if total > 500:
                raise ValueError("Select at most 500 individual MACs, or select the whole SSID")
            if not macs:
                if choice["mode"] == "include":
                    clauses.append("o.ssid<>?")
                    values.append(ssid)
                continue
            operator = "IN" if choice["mode"] == "include" else "NOT IN"
            clauses.append(f"(o.ssid<>? OR o.mac {operator} ({','.join('?' for _ in macs)}))")
            values.extend([ssid, *(mac.strip().upper() for mac in macs)])
    dates = observation_dates(params)
    for key, op in (("date_from", ">="), ("date_to", "<=")):
        if dates[key]:
            clauses.append(f"o.date {op} ?")
            values.append(dates[key])
    if dates["dates"]:
        clauses.append(f"o.date IN ({','.join('?' for _ in dates['dates'])})")
        values.extend(dates["dates"])
    if dates["date_from"] or dates["date_to"]:
        clauses.append("o.date<>''")
    minimum = float(params.get("min_rssi", ["-150"])[0])
    if not math.isfinite(minimum) or not -150 <= minimum <= 0:
        raise ValueError("RSSI must be between -150 and 0 dBm")
    clauses.append("o.rssi_dbm>=?")
    values.append(minimum)
    join = ""
    if spatial and params.get("bbox"):
        box = [float(v) for v in params["bbox"][0].split(",")]
        if len(box) != 4 or not all(math.isfinite(v) for v in box):
            raise ValueError("Invalid map bounds")
        w, s, e, n = box
        if not (-180 <= w <= e <= 180 and -85 <= s <= n <= 85):
            raise ValueError("Map bounds must be ordered within longitude ±180°, latitude ±85°")
        join = " JOIN spatial s ON s.id=o.id "
        clauses.extend(["s.min_lat<=?", "s.max_lat>=?", "s.min_lon<=?", "s.max_lon>=?",
                        "o.latitude BETWEEN ? AND ?", "o.longitude BETWEEN ? AND ?"])
        values.extend([n, s, e, w, s, n, w, e])
    return join, " AND ".join(clauses), values


def observation_dates(params):
    def canonical(value):
        if not value:
            return None
        try:
            parsed = date.fromisoformat(value)
        except (ValueError, TypeError) as exc:
            raise ValueError("Choose valid dates in YYYY-MM-DD format") from exc
        if parsed.isoformat() != value:
            raise ValueError("Choose dates in YYYY-MM-DD format")
        return value
    start = canonical(params.get("date_from", [""])[0])
    end = canonical(params.get("date_to", [""])[0])
    if start and end and start > end:
        raise ValueError("The start date must be on or before the end date")
    chosen = params.get("date", [])
    if len(chosen) > 1000:
        raise ValueError("Select at most 1,000 dates, or use a date range")
    if any(not day for day in chosen):
        raise ValueError("Select valid dates in YYYY-MM-DD format")
    chosen = sorted({canonical(day) for day in chosen if day})
    return {"date_from": start, "date_to": end, "dates": chosen}


def estimate_scope(params):
    mode = params.get("fit_mode", ["all"])[0]
    if mode not in ("all", "sample"):
        raise ValueError("Fit mode must be all or sample")
    minimum = float(params.get("min_rssi", ["-150"])[0])
    if not math.isfinite(minimum) or not -150 <= minimum <= 0:
        raise ValueError("RSSI must be between -150 and 0 dBm")
    scope = {"version": 2, "fit_mode": mode, "min_rssi": minimum, **observation_dates(params)}
    return scope, json.dumps(scope, sort_keys=True, separators=(",", ":"))


def available_dates(database, params):
    # Date options are scoped to the chosen radios, but not to a date window
    # which would otherwise make omitted dates impossible to select again.
    params = {k: v for k, v in params.items() if k not in ("date", "date_from", "date_to")}
    _, where, values = filters(params)
    with connect(database) as db:
        rows = db.execute(f"SELECT date,count(*) count FROM observations o WHERE {where} AND date<>'' GROUP BY date ORDER BY date", values).fetchall()
    result = []
    for row in rows:
        try:
            if date.fromisoformat(row["date"]).isoformat() == row["date"]:
                result.append(dict(row))
        except ValueError:
            continue
    return {"dates": result}


def summary(database, params=None):
    _, where, values = filters(params or {})
    with connect(database) as db:
        row = dict(db.execute(f'''SELECT count(*) count,count(DISTINCT ssid) networks,
          count(DISTINCT radio_key) radios,min(latitude) south,max(latitude) north,
          min(longitude) west,max(longitude) east,max(rssi_dbm) peak,min(date) first_date,max(date) last_date
          FROM observations o WHERE {where}''', values).fetchone())
        row["bands"] = {r["band"]: r["count"] for r in db.execute(f"SELECT band,count(*) count FROM observations o WHERE {where} GROUP BY band", values)}
        row["imports"] = [dict(r) for r in db.execute("SELECT filename,valid_rows,invalid_rows,non_wifi_rows FROM imports ORDER BY id")]
        return row


def page_limit(params, default=50, maximum=200):
    limit = max(1, min(maximum, int(params.get("limit", [str(default)])[0])))
    offset = max(0, int(params.get("offset", ["0"])[0]))
    return limit, offset


def networks(database, params):
    limit, offset = page_limit(params)
    needle = params.get("q", [""])[0].replace("\\", "\\\\").replace("%", "\\%").replace("_", "\\_")
    with connect(database) as db:
        rows = db.execute('''SELECT * FROM network_catalog WHERE ssid LIKE ? ESCAPE '\\'
          ORDER BY count DESC,ssid LIMIT ? OFFSET ?''', (f"%{needle}%", limit+1, offset)).fetchall()
    return {"rows": [dict(r) for r in rows[:limit]], "more": len(rows) > limit, "offset": offset}


def transmitters(database, params):
    """Lazy, paginated MAC children of exactly one SSID; bands remain distinct."""
    ssids = params.get("ssid", [])
    if len(ssids) != 1:
        raise ValueError("Expand exactly one SSID to list its MAC addresses")
    limit, offset = page_limit(params, 25)
    needle = params.get("q", [""])[0].strip().upper().replace("\\", "\\\\").replace("%", "\\%").replace("_", "\\_")
    with connect(database) as db:
        rows = db.execute('''SELECT t.*,
          (SELECT band FROM band_assignments a WHERE a.ssid=t.ssid AND a.mac=t.mac) manual_band,
          (SELECT count(*) FROM observations o WHERE o.ssid=t.ssid AND o.mac=t.mac
           AND o.band_source IN ('frequency/band unavailable','manual assignment')) editable_band_count
          FROM transmitter_catalog t WHERE ssid=? AND mac LIKE ? ESCAPE '\\'
          ORDER BY count DESC,mac LIMIT ? OFFSET ?''', (ssids[0], f"%{needle}%", limit+1, offset)).fetchall()
    return {"ssid": ssids[0], "rows": [dict(r) for r in rows[:limit]], "more": len(rows)>limit, "offset": offset}


def assign_band(database, ssid, mac, band):
    """Fill absent band metadata with a user declaration; measured data wins."""
    if not isinstance(ssid,str) or not isinstance(mac,str):
        raise ValueError('Choose an SSID and MAC address')
    mac=mac.strip().upper()
    if band not in ('','2.4','5','6'):
        raise ValueError('Choose 2.4, 5, or 6 GHz, or clear the manual assignment')
    with connect(database) as db:
        if not db.execute('SELECT 1 FROM observations WHERE ssid=? AND mac=?',(ssid,mac)).fetchone():
            raise ValueError('This SSID/MAC is not in the database')
        if band:
            db.execute('INSERT OR REPLACE INTO band_assignments VALUES(?,?,?,?)',
                       (ssid,mac,band,datetime.now(timezone.utc).isoformat()))
        else:
            db.execute('DELETE FROM band_assignments WHERE ssid=? AND mac=?',(ssid,mac))
        assigned=band or 'unknown'
        cursor=db.execute('''UPDATE observations SET band=?,band_source=?,radio_key=?
          WHERE ssid=? AND mac=? AND band_source IN ('frequency/band unavailable','manual assignment')''',
          (assigned,'manual assignment' if band else 'frequency/band unavailable',
           json.dumps([ssid,mac,assigned],separators=(',',':'),ensure_ascii=False),ssid,mac))
        if cursor.rowcount:
            # These are disposable computations. Historical comparison pins
            # remain immutable, including their original band declarations.
            db.execute('DELETE FROM estimates')
        return {'updated':cursor.rowcount,'band':assigned,'manual':bool(band)}


def radios(database, params):
    _, where, values = filters(params)
    limit, offset = page_limit(params)
    with connect(database) as db:
        rows = db.execute(f'''SELECT radio_key,ssid,mac,band,count(*) count,max(rssi_dbm) peak,
           min(frequency_mhz) frequency_mhz,group_concat(DISTINCT band_source) band_sources,
           (SELECT bands FROM transmitter_catalog t WHERE t.ssid=o.ssid AND t.mac=o.mac) seen_bands
           FROM observations o WHERE {where}
           GROUP BY radio_key ORDER BY count DESC,radio_key LIMIT ? OFFSET ?''', [*values, limit+1, offset]).fetchall()
    return {"rows": [dict(r) for r in rows[:limit]], "more": len(rows) > limit, "offset": offset}


def observations(database, params):
    join, where, values = filters(params, spatial=True)
    limit, offset = page_limit(params, 50, 100)
    with connect(database) as db:
        rows = db.execute(f"SELECT o.* FROM observations o {join} WHERE {where} ORDER BY o.id LIMIT ? OFFSET ?", [*values, limit+1, offset]).fetchall()
    return {"rows": [dict(r) for r in rows[:limit]], "more": len(rows) > limit, "offset": offset}


def points(database, params):
    join, where, values = filters(params, spatial=True)
    limit = max(100, min(5000, int(params.get("limit", ["2000"])[0])))
    with connect(database) as db:
        counts = dict(db.execute(f"SELECT count(*) count,min(latitude) south,max(latitude) north,min(longitude) west,max(longitude) east FROM observations o {join} WHERE {where}", values).fetchone())
        if counts["count"] <= limit:
            rows = db.execute(f"SELECT o.id,ssid,mac,band,latitude,longitude,rssi_dbm,accuracy_m,first_seen FROM observations o {join} WHERE {where} ORDER BY o.id", values).fetchall()
            return {"aggregated": False, "count": counts["count"], "points": [dict(r) for r in rows]}
        bins = int(math.sqrt(limit))
        dy = max(counts["north"]-counts["south"], .0000001)/bins
        dx = max(counts["east"]-counts["west"], .0000001)/bins
        # At most bins² cells, with centroids explicitly labeled as aggregates.
        rows = db.execute(f'''SELECT avg(latitude) latitude,avg(longitude) longitude,count(*) count,
          max(rssi_dbm) rssi_dbm,min(latitude) south,max(latitude) north,min(longitude) west,max(longitude) east,
          count(DISTINCT radio_key) radios,count(DISTINCT ssid) networks,min(ssid) ssid,
          count(DISTINCT band) bands,min(band) band FROM observations o {join} WHERE {where}
          GROUP BY min({bins-1},CAST((latitude-?)/? AS INTEGER)),min({bins-1},CAST((longitude-?)/? AS INTEGER))''',
          [*values, counts["south"], dy, counts["west"], dx]).fetchall()
    return {"aggregated": True, "count": counts["count"], "points": [dict(r) for r in rows]}
