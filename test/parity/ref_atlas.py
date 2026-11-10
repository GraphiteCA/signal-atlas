"""Run the parity scenario on Graphite's Python Signal Atlas and print every result as JSON."""
import json, sys, math
from pathlib import Path
HERE = Path(__file__).resolve()
sys.path.insert(0, str(HERE.parents[2] / "python"))
import survey_data as S, selection_estimates as SEL, area_estimates as AREA, estimate_ap as EA, serve_survey as SS

def safe(fn, *a, **k):
    try: return fn(*a, **k)
    except Exception as e: return {"error": str(e), "type": type(e).__name__}

def main(work):
    work = Path(work); sc = json.loads((HERE.parent / "scenario.json").read_text())
    A, B = work / "py-a.sqlite3", work / "py-b.sqlite3"
    for p in (A, B):
        for s in ("", "-wal", "-shm"):
            Path(str(p) + s).unlink(missing_ok=True)
    out = {}
    out["import"] = S.import_csv(work / "survey.csv", A, skip_invalid=True)
    out["import_dup"] = S.import_csv(work / "survey.csv", A, skip_invalid=True)
    out["import_strict"] = safe(S.import_csv, work / "survey.csv", B)
    out["queries"] = []
    for p in sc["params"]:
        q = {}
        for name, fn in (("summary", S.summary), ("radios", S.radios), ("observations", S.observations), ("points", S.points), ("dates", S.available_dates), ("selected", SEL.selected_groups), ("scope", S.estimate_scope)):
            q[name] = safe(fn, A, p) if name != "scope" else safe(lambda x: list(S.estimate_scope(x)), p)
        q["points100"] = safe(S.points, A, {**p, "limit": ["100"]})
        out["queries"].append(q)
    out["networks"] = [S.networks(A, {}), S.networks(A, {"q": ["e"], "limit": ["2"]})]
    out["transmitters"] = [safe(S.transmitters, A, {"ssid": ["Mesh"]}), safe(S.transmitters, A, {"ssid": ["Exampé \U0001F437"], "q": ["aa:00"]}), safe(S.transmitters, A, {})]
    out["assign"] = [safe(S.assign_band, A, "Lonely", " aa:00:00:00:00:40", "5"), safe(S.assign_band, A, "Lonely", "AA:00:00:00:00:40", "7"), safe(S.assign_band, A, "Nope", "AA", "5")]
    sel = SEL.selected_groups(A, {})
    out["groups"] = {k: SEL.estimation_groups(sel, v) for k, v in (("plain", {}), ("shared", {"shared": ["1"]}), ("sharedonly", {"shared": ["1"], "individual": ["0"]}))}
    out["aliases"] = SEL.unknown_band_aliases(sel)
    jobs = SS.Jobs(A)
    out["fits"] = []
    for fp in sc["fitParams"]:
        groups = SEL.estimation_groups(SEL.selected_groups(A, fp), fp)
        scope, key = S.estimate_scope(fp)
        res = []
        for g in groups:
            r = jobs.fit_radio(g, fp)
            res.append(r)
            with S.connect(A) as db:
                db.execute("INSERT OR REPLACE INTO estimates(radio_key,scope_key,result) VALUES(?,?,?)", (g["radio_key"], key, json.dumps(r, allow_nan=False)))
        out["fits"].append(res)
        out.setdefault("fitted", []).append(SS.fitted_estimates(A, fp))
    out["area"] = [safe(AREA.area_estimates, A, p) for p in sc["area"]]
    out["area_plan"] = [safe(lambda p: list(AREA.area_plan(A, p, display=False)), p) for p in sc["area"]]
    out["compare_save"] = safe(SEL.save_comparison, A, sc["fitParams"][0], "January", SS.fitted_estimates)
    out["comparisons"] = SEL.comparisons(A)
    out["info"] = S.database_info(A)
    obs = EA.read_observations(HERE.parents[2] / "python" / "demo-survey.csv", ssid="ExampleNet")
    out["estimate_report"] = EA.estimate(obs, step_m=10)
    print(json.dumps(out, allow_nan=True, default=str))

main(sys.argv[1])
