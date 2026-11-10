"""Show saved AP locations in the viewport and plan bounded nearby fits."""
import json
import math

from selection_estimates import estimation_groups, independent_groups
from survey_data import connect, estimate_scope, filters

MIN_ZOOM = 16
# Limit discovery work, never the number of already fitted APs on the map.
MAX_LOCATIONS = 60
BATCH_SIZE = 8


def overview_targets(db, scoped, ssid, hidden_mac, individual, shared):
    _, where, values = filters(scoped)
    match = 'o.ssid=?' if ssid else 'o.ssid=? AND o.mac=?'
    identity = [ssid] if ssid else [ssid, hidden_mac]
    members = [dict(r) for r in db.execute(f"""SELECT radio_key,ssid,mac,band,count(*) count
        FROM observations o WHERE {match} AND ({where}) GROUP BY mac,band
        ORDER BY mac,band""", [*identity, *values])]
    targets = {g['radio_key']: g for g in independent_groups(members)} if individual else {}
    if shared:
        for g in estimation_groups(members, {'shared': ['1'], 'individual': ['0']}):
            targets[g['radio_key']] = g
    return targets


def visible_fits(db, scope_keys, bounds, targets_for):
    west, south, east, north = bounds
    exact, sample = scope_keys
    rows = db.execute("""SELECT e.radio_key,e.result FROM estimates e
        WHERE e.scope_key IN (?, ?) AND json_extract(e.result,'$.status')='fitted'
        AND json_extract(e.result,'$.longitude') BETWEEN ? AND ?
        AND json_extract(e.result,'$.latitude') BETWEEN ? AND ?
        AND (e.scope_key=? OR NOT EXISTS (SELECT 1 FROM estimates x
            WHERE x.radio_key=e.radio_key AND x.scope_key=?
            AND json_extract(x.result,'$.status')='fitted')) ORDER BY e.radio_key""",
        (exact, sample, west, east, south, north, exact, exact))
    fits = []
    for row in rows:
        fit = json.loads(row['result'])
        mac = fit.get('members', [fit])[0]['mac']
        if row['radio_key'] in targets_for(fit['ssid'], mac):
            fits.append(fit)
    return fits


def nearby_groups(db, nearby, bounds, individual, targets_for):
    join, where, values = filters(nearby, spatial=True)
    # Discover individual MACs only on request. Hidden networks use a MAC identity.
    identity = "o.ssid,o.mac" if individual else "o.ssid, CASE WHEN o.ssid='' THEN o.mac END"
    west, south, east, north = bounds
    candidates = list(db.execute(f"""SELECT o.ssid,o.mac,
        max(o.longitude BETWEEN ? AND ? AND o.latitude BETWEEN ? AND ?) in_view,
        max(o.rssi_dbm) peak FROM observations o {join} WHERE {where}
        GROUP BY {identity} ORDER BY in_view DESC,peak DESC,o.ssid,o.mac LIMIT ?""",
        [west, east, south, north, *values, MAX_LOCATIONS + 1]))
    groups = {}
    for row in candidates[:MAX_LOCATIONS]:
        for key, group in targets_for(row['ssid'], row['mac']).items():
            if not individual or group.get('mac') == row['mac'] or group.get('members') and group.get('co_location_assumed', True):
                groups[key] = group
    return list(groups.values()), len(candidates) > MAX_LOCATIONS


def area_plan(database, params, *, display=True, discover=True):
    scoped = {k: v for k, v in params.items() if k in
              ('ssid_query', 'band', 'min_rssi', 'date', 'date_from', 'date_to', 'none')}
    scoped['fit_mode'] = ['sample']
    scope, sample_key = estimate_scope(scoped)
    _, exact_key = estimate_scope({**scoped, 'fit_mode': ['all']})
    zoom = float(params.get('zoom', ['0'])[0])
    if not math.isfinite(zoom):
        raise ValueError('Invalid zoom')
    result = dict(estimates=[], more=False, missing_estimates=0, min_zoom=MIN_ZOOM,
                  limit=MAX_LOCATIONS, observation_scope=scope, zoom_required=zoom < MIN_ZOOM)
    if not params.get('bbox'):
        if zoom < MIN_ZOOM:
            return result, scoped, []
        raise ValueError('Map bounds are required')
    filters({**scoped, 'bbox': params['bbox']}, spatial=True)
    if scoped.get('none') == ['1']:
        return result, scoped, []
    bounds = tuple(map(float, params['bbox'][0].split(',')))
    west, south, east, north = bounds
    dy = 500 / 111195
    dx = dy / max(.08, math.cos(math.radians((south + north) / 2)))
    nearby = {**scoped, 'bbox': [','.join(map(str, (max(-180, west-dx), max(-85, south-dy),
                                                    min(180, east+dx), min(85, north+dy))))]}
    with connect(database) as db:
        individual = params.get('individual') == ['1']
        shared = not individual or params.get('shared') == ['1']
        target_cache = {}

        def targets_for(ssid, mac):
            identity = (ssid, '' if ssid else mac)
            if identity not in target_cache:
                target_cache[identity] = overview_targets(db, scoped, *identity, individual, shared)
            return target_cache[identity]

        if display:
            result['estimates'] = visible_fits(db, (exact_key, sample_key), bounds, targets_for)
        if not discover or zoom < MIN_ZOOM:
            return result, scoped, []
        groups, result['more'] = nearby_groups(db, nearby, bounds, individual, targets_for)
        keys = [g['radio_key'] for g in groups]
        cached = {r[0] for r in db.execute(f"""SELECT radio_key FROM estimates
            WHERE (scope_key=? OR (scope_key=? AND json_extract(result,'$.status')='fitted'))
            AND radio_key IN ({','.join('?' for _ in keys)})""", [sample_key, exact_key, *keys])} if keys else set()
    missing = [g for g in groups if g['radio_key'] not in cached]
    result['missing_estimates'] = len(missing)
    return result, scoped, missing[:BATCH_SIZE]


def area_estimates(database, params):
    return area_plan(database, params, discover=params.get('saved_only') != ['1'])[0]
