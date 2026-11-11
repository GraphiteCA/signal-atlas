package atlas

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"

	"signal-atlas/internal/db"
	"signal-atlas/internal/obs"
	"signal-atlas/internal/py"
	"zombiezen.com/go/sqlite"
)

// Target versions prove input validity independently of the map's global revision.
const preparationSchema = `
CREATE TABLE IF NOT EXISTS preparation_meta(name TEXT PRIMARY KEY,value INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS preparation_targets(target_key TEXT PRIMARY KEY,ssid TEXT NOT NULL,mac TEXT NOT NULL,version INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS preparation_namespaces(namespace TEXT PRIMARY KEY,scope_key TEXT NOT NULL,grouping TEXT NOT NULL,total INTEGER NOT NULL,pending INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS preparation_pending(namespace TEXT NOT NULL,target_key TEXT NOT NULL,version INTEGER NOT NULL,blocked_bytes INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(namespace,target_key));
CREATE INDEX IF NOT EXISTS preparation_ready ON preparation_pending(namespace,blocked_bytes,target_key);
CREATE TRIGGER IF NOT EXISTS preparation_enqueue AFTER INSERT ON preparation_pending BEGIN UPDATE preparation_namespaces SET pending=pending+1 WHERE namespace=new.namespace; END;
CREATE TRIGGER IF NOT EXISTS preparation_dequeue AFTER DELETE ON preparation_pending BEGIN UPDATE preparation_namespaces SET pending=pending-1 WHERE namespace=old.namespace; END;
CREATE TABLE IF NOT EXISTS preparation_checkpoints(namespace TEXT NOT NULL,target_key TEXT NOT NULL,version INTEGER NOT NULL,fitted INTEGER NOT NULL,unavailable INTEGER NOT NULL,completed_utc TEXT NOT NULL,PRIMARY KEY(namespace,target_key));
`

type PreparationSettings struct {
	Version  int    `json:"version"`
	Enabled  bool   `json:"enabled"`
	Paused   bool   `json:"paused"`
	FitMode  string `json:"fit_mode"`
	Grouping string `json:"grouping"`
	Workers  int    `json:"workers"`
	MemoryMB int    `json:"memory_mb"`
}

func DefaultPreparationSettings() PreparationSettings {
	return PreparationSettings{Version: 1, Enabled: true, FitMode: "sample", Grouping: "shared", Workers: max(1, min(4, runtime.GOMAXPROCS(0)/2)), MemoryMB: 256}
}
func (p PreparationSettings) Validate() error {
	if p.Version != 1 || (p.FitMode != "sample" && p.FitMode != "all") || (p.Grouping != "shared" && p.Grouping != "individual" && p.Grouping != "both") || p.Workers < 1 || p.Workers > 16 || p.MemoryMB < 64 || p.MemoryMB > 2048 {
		return py.Errorf("Choose a supported preparation mode, 1–16 calculations and 64–2048 MB working memory")
	}
	return nil
}
func (a *Atlas) PreparationSettings(c *sqlite.Conn) (PreparationSettings, error) {
	p := DefaultPreparationSettings()
	v, err := db.Scalar(c, "SELECT payload FROM map_assets WHERE name='preparation_settings'")
	if err != nil {
		return p, err
	}
	if v != nil {
		if err = json.Unmarshal([]byte(db.Str(v)), &p); err != nil {
			return p, err
		}
	}
	return p, p.Validate()
}
func (a *Atlas) SavePreparationSettings(c *sqlite.Conn, p PreparationSettings) error {
	if err := p.Validate(); err != nil {
		return err
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = db.Exec(c, "INSERT OR REPLACE INTO map_assets(name,payload) VALUES('preparation_settings',?)", string(b))
	return err
}
func TargetKey(ssid, mac string) string {
	if ssid != "" {
		return "n:" + ssid
	}
	return "h:" + strings.ToUpper(mac)
}
func groupTarget(g Group) string {
	mac := gs(g, "mac")
	if mac == "" {
		if ms := members(g); len(ms) > 0 {
			mac = gs(ms[0], "mac")
		}
	}
	return TargetKey(gs(g, "ssid"), mac)
}
func targetVersion(c *sqlite.Conn, key string) (int64, error) {
	v, e := db.Scalar(c, "SELECT version FROM preparation_targets WHERE target_key=?", key)
	return db.Int(v), e
}
func (a *Atlas) FitInputVersion(c *sqlite.Conn, g Group) (PreparationTarget, error) {
	key := groupTarget(g)
	ssid := gs(g, "ssid")
	mac := ""
	if ssid == "" {
		mac = strings.TrimPrefix(key, "h:")
	}
	v, err := targetVersion(c, key)
	return PreparationTarget{Key: key, SSID: ssid, MAC: mac, Version: v}, err
}

// Seed once from existing indexes, without rescanning observations. Completed
// workspaces reopen from their persisted queue; no TargetGroups sweep is needed.
func (a *Atlas) initializePreparationCatalog(c *sqlite.Conn) error {
	v, err := db.Scalar(c, "SELECT value FROM preparation_meta WHERE name='catalog'")
	if err != nil || v != nil {
		return err
	}
	return db.WriteTx(c, func() error {
		insert, err := c.Prepare("INSERT OR IGNORE INTO preparation_targets VALUES(?,?,?,1)")
		if err != nil {
			return err
		}
		defer insert.Finalize()
		add := func(ssid, mac string) error {
			if err := db.Bind(insert, []any{TargetKey(ssid, mac), ssid, mac}); err != nil {
				return err
			}
			if _, err := insert.Step(); err != nil {
				return err
			}
			return insert.Reset()
		}
		st := a.Obs.State()
		for id := range st.Net {
			if ssid := st.Str[id]; ssid != "" {
				if err := add(ssid, ""); err != nil {
					return err
				}
			}
		}
		for pair := range st.Trans {
			if st.Str[pair[0]] == "" {
				if err := add("", st.Str[pair[1]]); err != nil {
					return err
				}
			}
		}
		_, err = db.Exec(c, "INSERT INTO preparation_meta VALUES('catalog',1)")
		return err
	})
}

// Called inside the observation writer's metadata transaction. Queues and fit
// invalidation commit together. Untouched targets keep versions and checkpoints.
func dirtyPreparationTargets(c *sqlite.Conn) error {
	// Reuse the writer's affected-identity temp tables. Bulk SQL avoids several
	// Go/SQLite round trips per network on high-cardinality multi-million imports.
	return db.Script(c, `
CREATE TEMP TABLE IF NOT EXISTS stale_preparation(target_key TEXT PRIMARY KEY,ssid TEXT NOT NULL,mac TEXT NOT NULL);
DELETE FROM stale_preparation;
INSERT INTO stale_preparation SELECT 'n:'||v,v,'' FROM stale_ssid;
INSERT OR IGNORE INTO stale_preparation SELECT 'h:'||upper(v),'',upper(v) FROM stale_mac;
INSERT INTO preparation_targets SELECT target_key,ssid,mac,1 FROM stale_preparation WHERE true
ON CONFLICT(target_key) DO UPDATE SET version=version+1;
UPDATE preparation_namespaces SET total=(SELECT count(*) FROM preparation_targets);
INSERT INTO preparation_pending(namespace,target_key,version)
SELECT n.namespace,t.target_key,t.version FROM preparation_namespaces n CROSS JOIN stale_preparation d JOIN preparation_targets t USING(target_key) WHERE true
ON CONFLICT(namespace,target_key) DO UPDATE SET version=excluded.version,blocked_bytes=0;
DELETE FROM stale_preparation;
`)
}

type PreparationNamespace struct {
	ID       string    `json:"id"`
	ScopeKey string    `json:"scope_key"`
	Solver   string    `json:"solver"`
	Grouping string    `json:"grouping"`
	FitMode  string    `json:"fit_mode"`
	Params   py.Params `json:"-"`
}

func NewPreparationNamespace(p PreparationSettings, fast bool, accelerated ...bool) (PreparationNamespace, error) {
	if err := p.Validate(); err != nil {
		return PreparationNamespace{}, err
	}
	solver := SolverName(fast, len(accelerated) > 0 && accelerated[0])
	params := py.Params{"fit_mode": {p.FitMode}, "min_rssi": {"-150"}, "solver": {solver}}
	_, scope, err := EstimateScope(params)
	return PreparationNamespace{ID: shaJSON([]string{"preparation-v1", scope, p.Grouping}), ScopeKey: scope, Solver: solver, Grouping: p.Grouping, FitMode: p.FitMode, Params: params}, err
}
func (a *Atlas) EnsurePreparationNamespace(ctx context.Context, ns PreparationNamespace) error {
	a.WriteMu.Lock()
	defer a.WriteMu.Unlock()
	c, put, err := a.Conn(ctx)
	if err != nil {
		return err
	}
	defer put()
	v, err := db.Scalar(c, "SELECT 1 FROM preparation_namespaces WHERE namespace=?", ns.ID)
	if err != nil || v != nil {
		return err
	}
	return db.WriteTx(c, func() error {
		if _, err := db.Exec(c, "INSERT INTO preparation_namespaces(namespace,scope_key,grouping,total) SELECT ?,?,?,count(*) FROM preparation_targets", ns.ID, ns.ScopeKey, ns.Grouping); err != nil {
			return err
		}
		_, err := db.Exec(c, "INSERT INTO preparation_pending(namespace,target_key,version) SELECT ?,target_key,version FROM preparation_targets", ns.ID)
		return err
	})
}

type PreparationTarget struct {
	Key     string `json:"key"`
	SSID    string `json:"ssid"`
	MAC     string `json:"mac"`
	Version int64  `json:"version"`
}
type PreparationProgress struct {
	Total     int64 `json:"total"`
	Pending   int64 `json:"pending"`
	Blocked   int64 `json:"blocked"`
	Completed int64 `json:"completed"`
}

func (a *Atlas) PreparationProgress(ctx context.Context, ns string, budget int64) (PreparationProgress, error) {
	c, put, err := a.Conn(ctx)
	if err != nil {
		return PreparationProgress{}, err
	}
	defer put()
	r, err := db.One(c, `SELECT total,pending,(SELECT count(*) FROM preparation_pending WHERE namespace=? AND blocked_bytes>?) blocked FROM preparation_namespaces WHERE namespace=?`, ns, budget, ns)
	if err != nil {
		return PreparationProgress{}, err
	}
	p := PreparationProgress{Total: db.Int(r["total"]), Pending: db.Int(r["pending"]), Blocked: db.Int(r["blocked"])}
	p.Completed = p.Total - p.Pending
	return p, nil
}
func (a *Atlas) NextPreparationTargets(ctx context.Context, ns string, budget int64, limit int) ([]PreparationTarget, error) {
	c, put, err := a.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer put()
	rows, err := db.All(c, `SELECT t.* FROM preparation_pending p JOIN preparation_targets t USING(target_key) WHERE p.namespace=? AND p.blocked_bytes<=? ORDER BY p.blocked_bytes,p.target_key LIMIT ?`, ns, budget, limit)
	if err != nil {
		return nil, err
	}
	var out []PreparationTarget
	for _, r := range rows {
		out = append(out, PreparationTarget{Key: db.Str(r["target_key"]), SSID: db.Str(r["ssid"]), MAC: db.Str(r["mac"]), Version: db.Int(r["version"])})
	}
	return out, nil
}

var ErrStaleInput = errors.New("Target inputs changed; calculation was not saved")
var ErrPreparationBudget = errors.New("Target exceeds background working memory budget")

type PreparedTargetResult struct {
	Fitted      int `json:"fitted"`
	Unavailable int `json:"unavailable"`
	Reused      int `json:"reused"`
	Written     int `json:"written"`
}

// PreparedTarget contains bounded calculation output, not a database lease.
// A result can only be published after its input version is rechecked by the
// single batch writer. Blocked targets retain pending work rather than a checkpoint.
type PreparedEstimate struct{ Key, Text string }
type PreparedTarget struct {
	Target       PreparationTarget
	Result       PreparedTargetResult
	Writes       []PreparedEstimate
	BlockedBytes int64
}

func (a *Atlas) ComputePreparationTarget(ctx context.Context, ns PreparationNamespace, t PreparationTarget, budget int64) (PreparedTarget, error) {
	return a.computePreparationTarget(ctx, ns, t, budget, true)
}

// ComputeUncachedPreparationTarget shares the exact grouping, sampling and Go
// solver with the native store for bounded external observation snapshots.
// Only use it for fresh work; normal preparation preserves existing fit caches.
func (a *Atlas) ComputeUncachedPreparationTarget(ctx context.Context, ns PreparationNamespace, t PreparationTarget, budget int64) (PreparedTarget, error) {
	return a.computePreparationTarget(ctx, ns, t, budget, false)
}
func (a *Atlas) computePreparationTarget(ctx context.Context, ns PreparationNamespace, t PreparationTarget, budget int64, useCache bool) (PreparedTarget, error) {
	prepared := PreparedTarget{Target: t}
	result := &prepared.Result
	if err := ctx.Err(); err != nil {
		return prepared, err
	}
	st := a.Obs.State()
	sid, hasSSID := a.Obs.Dict.Lookup(t.SSID)
	var rows int64
	if hasSSID {
		if t.SSID != "" {
			if n := st.Net[sid]; n != nil {
				rows = n.Count
			}
		} else if mid, ok := a.Obs.Dict.Lookup(t.MAC); ok {
			if n := st.Trans[obs.Pair{sid, mid}]; n != nil {
				rows = n.Count
			}
		}
	}
	if ns.Grouping == "both" {
		rows *= 2
	}
	// Includes grouping/sample scratch, not the already loaded column store.
	needed := rows*256 + (1 << 20)
	if needed > budget {
		prepared.BlockedBytes = needed
		return prepared, nil
	}
	tm, err := a.overviewTargetsContext(ctx, ns.Params, t.SSID, t.MAC, ns.Grouping != "shared", ns.Grouping != "individual")
	if err != nil {
		return prepared, err
	}
	var cached map[string]string
	if useCache {
		cached, err = a.preparationCache(ctx, ns, tm.keys)
		if err != nil {
			return prepared, err
		}
	}
	var writes []PreparedEstimate
	if len(tm.keys) == 0 {
		result.Unavailable = 1
	} // no group meets eligibility for these inputs
	for _, k := range tm.keys {
		if ctx.Err() != nil {
			return prepared, ctx.Err()
		}
		g := tm.m[k]
		v := cached[k]
		var fit map[string]any
		if v != "" {
			if err := json.Unmarshal([]byte(v), &fit); err != nil {
				return prepared, err
			}
			result.Reused++
		} else {
			fit, err = a.FitRadio(g, ns.Params, func() bool { return ctx.Err() != nil })
			if err != nil {
				return prepared, err
			}
			text, err := py.Dumps(fit, py.DumpOpts{})
			if err != nil {
				return prepared, err
			}
			writes = append(writes, PreparedEstimate{k, text})
			result.Written++
		}
		if fit["status"] == "fitted" {
			result.Fitted++
		} else {
			result.Unavailable++
		}
	}

	prepared.Writes = writes
	return prepared, nil
}

// Release the metadata lease before the CPU solver. Worker counts can exceed
// idle pooled connections without starving map/status reads or the batch writer.
func (a *Atlas) preparationCache(ctx context.Context, ns PreparationNamespace, keys []string) (map[string]string, error) {
	c, put, err := a.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer put()
	cache := make(map[string]string, len(keys))
	for _, key := range keys {
		q := "SELECT result FROM estimates WHERE radio_key=? AND scope_key=?"
		v, err := db.Scalar(c, q, key, ns.ScopeKey)
		if err != nil {
			return nil, err
		}
		if v == nil && ns.FitMode == "sample" {
			_, exact, _ := EstimateScope(ns.Params.With("fit_mode", []string{"all"}))
			v, err = db.Scalar(c, q+" AND json_extract(result,'$.status')='fitted'", key, exact)
			if err != nil {
				return nil, err
			}
		}
		if v != nil {
			cache[key] = db.Str(v)
		}
	}
	return cache, nil
}

// CommitPreparationBatch is the single preparation writer. Every accepted
// target's locations and checkpoint share one immediate transaction. Stale
// targets are skipped individually, leaving their new versions pending.
func (a *Atlas) CommitPreparationBatch(ctx context.Context, ns PreparationNamespace, batch []PreparedTarget) ([]PreparedTarget, error) {
	if a.beforePreparationCommit != nil {
		a.beforePreparationCommit()
	}
	a.WriteMu.Lock()
	defer a.WriteMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c, put, err := a.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer put()
	return CommitPreparationResults(ctx, c, ns, batch)
}

// CommitPreparationResults is shared by the production single writer and the
// benchmark observation adapters. The caller owns a dedicated connection and
// serializes writes; the immediate transaction always validates input versions.
func CommitPreparationResults(ctx context.Context, c *sqlite.Conn, ns PreparationNamespace, batch []PreparedTarget) ([]PreparedTarget, error) {
	accepted := make([]PreparedTarget, 0, len(batch))
	err := db.WriteTx(c, func() error {
		for _, prepared := range batch {
			if err := ctx.Err(); err != nil {
				return err
			}
			t := prepared.Target
			version, err := db.CachedScalar(c, "SELECT version FROM preparation_targets WHERE target_key=?", t.Key)
			if err != nil {
				return err
			}
			if db.Int(version) != t.Version {
				continue
			}
			pending, err := db.CachedScalar(c, "SELECT 1 FROM preparation_pending WHERE namespace=? AND target_key=? AND version=?", ns.ID, t.Key, t.Version)
			if err != nil {
				return err
			}
			if pending == nil {
				continue
			}
			if prepared.BlockedBytes > 0 {
				if _, err = db.CachedExec(c, "UPDATE preparation_pending SET blocked_bytes=? WHERE namespace=? AND target_key=? AND version=?", prepared.BlockedBytes, ns.ID, t.Key, t.Version); err != nil {
					return err
				}
			} else {
				for _, w := range prepared.Writes {
					if _, err = db.CachedExec(c, "INSERT OR REPLACE INTO estimates VALUES(?,?,?)", w.Key, ns.ScopeKey, w.Text); err != nil {
						return err
					}
				}
				if _, err = db.CachedExec(c, "INSERT OR REPLACE INTO preparation_checkpoints VALUES(?,?,?,?,?,?)", ns.ID, t.Key, t.Version, prepared.Result.Fitted, prepared.Result.Unavailable, py.UTCNowISO()); err != nil {
					return err
				}
				if _, err = db.CachedExec(c, "DELETE FROM preparation_pending WHERE namespace=? AND target_key=? AND version=?", ns.ID, t.Key, t.Version); err != nil {
					return err
				}
			}
			accepted = append(accepted, prepared)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("preparation commit (%s): %w", sqlite.ErrCode(err), err)
	}
	return accepted, nil
}

// PrepareTarget preserves the individual API for manual callers/tests while the
// automatic runner uses ComputePreparationTarget and CommitPreparationBatch.
func (a *Atlas) PrepareTarget(ctx context.Context, ns PreparationNamespace, t PreparationTarget, budget int64) (PreparedTargetResult, error) {
	prepared, err := a.ComputePreparationTarget(ctx, ns, t, budget)
	if err != nil {
		return prepared.Result, err
	}
	accepted, err := a.CommitPreparationBatch(ctx, ns, []PreparedTarget{prepared})
	if err != nil {
		return prepared.Result, err
	}
	if len(accepted) == 0 {
		return prepared.Result, ErrStaleInput
	}
	if prepared.BlockedBytes > 0 {
		return prepared.Result, ErrPreparationBudget
	}
	return prepared.Result, nil
}

func (n PreparationNamespace) String() string {
	return fmt.Sprintf("%s/%s/%s", n.Solver, n.FitMode, n.Grouping)
}
