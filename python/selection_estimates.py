"""Selection-aware fit groups and immutable, local comparison snapshots."""
from collections import defaultdict
from datetime import datetime, timezone
import hashlib
import json
import uuid

from survey_data import connect, filters


def selected_groups(database, params):
    _, where, values = filters(params)
    with connect(database) as db:
        return [dict(r) for r in db.execute(f'''SELECT radio_key,ssid,mac,band,count(*) count
            FROM observations o WHERE {where} GROUP BY radio_key ORDER BY radio_key''', values)]


def unknown_band_aliases(groups):
    by_mac = defaultdict(list)
    for group in groups:
        by_mac[(group['ssid'], group['mac'])].append(group)
    aliases = {}
    for members in by_mac.values():
        known = [g for g in members if g['band'] != 'unknown']
        if len(known) == 1:
            for group in members:
                if group['band'] == 'unknown':
                    aliases[group['radio_key']] = known[0]['radio_key']
    return aliases


def joint_groups(groups, params):
    if params.get('shared') != ['1']:
        return []
    by_ssid = defaultdict(list)
    for group in groups:
        by_ssid[group['ssid']].append(group)
    result = []
    for ssid, members in by_ssid.items():
        if len(members) < 2:
            continue
        if len(members) > 20:
            # Broadening a map selection must remain usable even with Shared
            # enabled. The UI reports that large SSIDs need narrowing.
            continue
        keys = sorted(r['radio_key'] for r in members)
        bands = sorted({r['band'] for r in members})
        aliases = unknown_band_aliases(members)
        identity = [keys, sorted(aliases.items())] if aliases else keys
        key = 'shared:' + hashlib.sha256(json.dumps(identity).encode()).hexdigest()
        mac_count = len({r['mac'] for r in members})
        result.append({'radio_key': key, 'ssid': ssid, 'mac': f'{mac_count} MAC address' + ('es' if mac_count != 1 else ''),
                       'band': bands[0] if len(bands)==1 else 'multiple',
                       'members': members, 'count': sum(r['count'] for r in members)})
    return result


def independent_groups(groups):
    """Unknown band metadata does not create another transmitter for a known MAC."""
    by_mac = defaultdict(list)
    for group in groups:
        by_mac[group['ssid'], group['mac']].append(group)
    result = []
    for members in by_mac.values():
        known = [g for g in members if g['band'] != 'unknown']
        if len(known) == 1 and len(members) > 1:
            keys = sorted(g['radio_key'] for g in members)
            key = 'radio:' + hashlib.sha256(json.dumps(keys).encode()).hexdigest()
            result.append({**known[0], 'radio_key': key, 'members': members,
                           'count': sum(g['count'] for g in members), 'co_location_assumed': False})
        else:
            # With several recorded bands an unknown observation cannot be assigned.
            result.extend(known or members)
    return result


def estimation_groups(groups, params):
    """Choose displayed/computed models without changing their cached identity.

    The viewer requests shared-only fits with individual=0. Older API callers
    keep their explicit independent/both behavior. A one-radio SSID needs only
    its ordinary radio fit, even when using the shared-location view.
    """
    shared = joint_groups(groups, params)
    shared_only = params.get('shared') == ['1'] and params.get('individual') == ['0']
    if shared_only:
        counts = defaultdict(int)
        for group in groups:
            counts[group['ssid']] += 1
        independent = [g for g in groups if counts[g['ssid']]==1 and g['count']>=4]
    else:
        independent = [g for g in independent_groups(groups) if g['count']>=4]
    return independent + shared


def annotate_bands(db, results):
    for result in results:
        row = db.execute('SELECT bands FROM network_catalog WHERE ssid=?', (result['ssid'],)).fetchone()
        result['ssid_seen_bands'] = row['bands'].split(',') if row else []
        row = db.execute('SELECT bands FROM transmitter_catalog WHERE ssid=? AND mac=?',
                         (result['ssid'], result['mac'])).fetchone()
        result['seen_bands'] = (row['bands'].split(',') if row else
                               sorted({r['band'] for r in result.get('members', [])}))


def comparisons(database):
    with connect(database) as db:
        return {'comparisons': [dict(id=r['id'], name=r['name'], created_utc=r['created_utc'],
                                     **json.loads(r['payload']))
                                for r in db.execute('SELECT * FROM comparisons ORDER BY created_utc,id')]}


def save_comparison(database, params, name, get_estimates):
    name = str(name or '').strip()[:100]
    if not name:
        raise ValueError('Name this comparison, for example January or Before move')
    result = get_estimates(database, {**params, 'limit': ['61']})
    if result['missing_estimates']:
        raise ValueError('Wait for the selected estimates to finish before pinning this date range')
    fits = [r for r in result['estimates'] if r['status']=='fitted']
    if result['more'] or len(result['estimates']) > 60:
        raise ValueError('Narrow the selection to at most 60 candidates before pinning a comparison')
    if not fits:
        raise ValueError('Estimate the selected radios before pinning a comparison')
    identifier = uuid.uuid4().hex
    payload = {'estimates': fits, 'observation_scope': result['observation_scope'], 'filters': params}
    with connect(database) as db:
        if db.execute('SELECT count(*) FROM comparisons').fetchone()[0] >= 6:
            raise ValueError('Keep up to six pinned comparisons; remove one before adding another')
        db.execute('INSERT INTO comparisons VALUES(?,?,?,?)',
                   (identifier, name, datetime.now(timezone.utc).isoformat(), json.dumps(payload, allow_nan=False)))
    return identifier


def delete_comparison(database, identifier):
    with connect(database) as db:
        db.execute('DELETE FROM comparisons WHERE id=?', (identifier,))
