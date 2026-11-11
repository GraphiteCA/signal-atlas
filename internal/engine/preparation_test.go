package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"signal-atlas/internal/atlas"
	"signal-atlas/internal/db"
)

func preparationServer(t *testing.T) *Server {
	t.Helper()
	s, err := New(Options{Data: filepath.Join(t.TempDir(), "data"), Version: "dev", UI: os.DirFS("../../ui")})
	if err != nil {
		t.Fatal(err)
	}
	s.AllowHost("admin.test")
	t.Cleanup(s.Close)
	return s
}
func addPreparationRows(t *testing.T, s *Server, n int) {
	t.Helper()
	var text strings.Builder
	text.WriteString("MAC,SSID,FirstSeen,Frequency,RSSI,CurrentLatitude,CurrentLongitude,AccuracyMeters,Type\n")
	for net := 0; net < n; net++ {
		for i := 0; i < 8; i++ {
			fmt.Fprintf(&text, "02:70:00:00:00:%02X,Auto%d,2026-11-01 00:00:%02d,2412,%d,%.6f,%.6f,3,WIFI\n", net, net, i, -55-i, 48.4+float64(i)*0.0001, -123.4+float64(i%3)*0.0001)
		}
	}
	file := filepath.Join(t.TempDir(), "preparation.csv")
	if err := os.WriteFile(file, []byte(text.String()), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Atlas.ImportCSV(file, false, nil, "", nil); err != nil {
		t.Fatal(err)
	}
}
func preparationStatus(t *testing.T, s *Server) map[string]any {
	t.Helper()
	w := adminRequest(t, s, "GET", "/api/app/preparation/status", nil, "")
	var v map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	return v
}
func waitPreparation(t *testing.T, s *Server, status string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		v := preparationStatus(t, s)
		if v["status"] == status {
			return v
		}
		if v["status"] == "failed" {
			t.Fatal(v)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("preparation timeout", preparationStatus(t, s))
	return nil
}
func savePreparation(t *testing.T, s *Server, body map[string]any) {
	t.Helper()
	w := adminRequest(t, s, "POST", "/api/app/preparation/settings", body, "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestAutomaticPreparationCompletesAndReopensWithoutTargetWalk(t *testing.T) {
	s := preparationServer(t)
	addPreparationRows(t, s, 4)
	state := waitPreparation(t, s, "complete")
	if state["progress"].(map[string]any)["completed"] != float64(4) {
		t.Fatal(state)
	}
	c, put, _ := s.Atlas.Conn(nil)
	n, err := db.Scalar(c, "SELECT count(*) FROM estimates WHERE json_extract(result,'$.status')='fitted'")
	put()
	if err != nil || db.Int(n) != 4 {
		t.Fatal(n, err)
	}
	savePreparation(t, s, map[string]any{"paused": true})
	waitPreparation(t, s, "paused")
	if !preparationStatus(t, s)["settings"].(map[string]any)["paused"].(bool) {
		t.Fatal("pause not persisted")
	}
	savePreparation(t, s, map[string]any{"paused": false})
	waitPreparation(t, s, "complete")
	data := s.o.Data
	s.Close()
	reopened, err := New(Options{Data: data, Version: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopened.AllowHost("admin.test")
	var calculated atomic.Int64
	reopened.preparation.mu.Lock()
	real := reopened.preparation.prepare
	reopened.preparation.prepare = func(ctx context.Context, n atlas.PreparationNamespace, target atlas.PreparationTarget, b int64) (atlas.PreparedTarget, error) {
		calculated.Add(1)
		return real(ctx, n, target, b)
	}
	reopened.preparation.mu.Unlock()
	waitPreparation(t, reopened, "complete")
	if calculated.Load() != 0 {
		t.Fatal("completed reopen walked/calculated targets")
	}
	// A solver/mode change selects independent durable work; display changes don't.
	w := adminRequest(t, reopened, "POST", "/api/app/settings", map[string]any{"fast_fits": true}, "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	savePreparation(t, reopened, map[string]any{"fit_mode": "all"})
	deadline := time.Now().Add(5 * time.Second)
	for {
		state = preparationStatus(t, reopened)
		ns, _ := state["namespace"].(map[string]any)
		if state["status"] == "complete" && ns["solver"] == "fast" && ns["fit_mode"] == "all" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(state)
		}
		time.Sleep(10 * time.Millisecond)
	}
	before := calculated.Load()
	adminRequest(t, reopened, "POST", "/api/app/map/display", map[string]any{"clustering": false, "label_min_zoom": 2, "radius_m": 50, "rings": false}, "")
	time.Sleep(300 * time.Millisecond)
	if calculated.Load() != before {
		t.Fatal("display change scheduled fits")
	}
}
func TestPreparationYieldsToForegroundAndJoinsOnSwitch(t *testing.T) {
	s := preparationServer(t)
	addPreparationRows(t, s, 2)
	started := make(chan struct{})
	var once sync.Once
	s.preparation.mu.Lock()
	s.preparation.prepare = func(ctx context.Context, _ atlas.PreparationNamespace, _ atlas.PreparationTarget, _ int64) (atlas.PreparedTarget, error) {
		once.Do(func() { close(started) })
		<-ctx.Done()
		return atlas.PreparedTarget{}, fmt.Errorf("sqlite: step: interrupted")
	}
	s.preparation.mu.Unlock()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("background never started")
	}
	// The first unclustered page drains in-flight commits. Quiet intervals then
	// keep its subsequent cursors stable while the view progressively loads.
	mapStart := time.Now()
	w := adminRequest(t, s, "GET", "/api/app/map/features?clustering=0&zoom=15&bbox=-124,48,-123,49", nil, "")
	if w.Code != 200 || time.Since(mapStart) > time.Second {
		t.Fatal("paged map did not preempt preparation", w.Code, w.Body.String())
	}
	t0 := time.Now()
	savePreparation(t, s, map[string]any{"paused": true})
	if time.Since(t0) > time.Second {
		t.Fatal("foreground cancellation exceeded 1 second")
	}
	waitPreparation(t, s, "paused")
	if state := preparationStatus(t, s); state["error"] != "" {
		t.Fatal("cooperative SQLite cancellation reported as failure", state)
	}
	savePreparation(t, s, map[string]any{"paused": false})
	old := s.preparation
	requireComplete(t, adminAction(t, s, "new", map[string]any{"path": filepath.Join(t.TempDir(), "new.atlas")}))
	select {
	case <-old.done:
	default:
		t.Fatal("old preparation was not joined")
	}
	if old == s.preparation || old.surveyID == s.surveyID {
		t.Fatal("runner crossed survey sessions")
	}
	if s.events.snapshot(0, s.surveyID)["survey_id"] != s.surveyID {
		t.Fatal("event scope")
	}
}
func TestPreparationEventsAfterCommitAndBoundedSlowSink(t *testing.T) {
	s := preparationServer(t)
	addPreparationRows(t, s, 3)
	delivered := make(chan JobEvent, 4)
	s.SetJobEventSink(JobEventSinkFunc(func(e JobEvent) { delivered <- e }))
	waitPreparation(t, s, "complete")
	select {
	case e := <-delivered:
		if e.SurveyID != s.surveyID || e.JobID != s.preparation.id || e.Solver != "reference" || len(e.Targets) == 0 {
			t.Fatal(e)
		}
		c, put, _ := s.Atlas.Conn(nil)
		for _, target := range e.Targets {
			v, err := db.Scalar(c, "SELECT version FROM preparation_checkpoints WHERE namespace=? AND target_key=?", e.Namespace, target.Key)
			if err != nil || db.Int(v) != target.Version {
				t.Fatal("event preceded commit", e, v, err)
			}
		}
		put()
	case <-time.After(500 * time.Millisecond):
		t.Fatal("committed fit event not delivered within 500 ms")
	}
	h := newJobEvents()
	defer h.close()
	blocked, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	h.mu.Lock()
	h.sink = JobEventSinkFunc(func(JobEvent) { once.Do(func() { close(blocked) }); <-release })
	h.mu.Unlock()
	h.publish(JobEvent{SurveyID: "one", JobID: "job"})
	<-blocked
	t0 := time.Now()
	for i := 0; i < 10000; i++ {
		h.publish(JobEvent{SurveyID: "one", JobID: "job", Targets: []atlas.PreparationTarget{{Key: fmt.Sprint(i), Version: 1}}})
	}
	if time.Since(t0) > time.Second {
		t.Fatal("slow sink blocked producer")
	}
	h.mu.Lock()
	ring, latest := len(h.ring), len(h.latest.Targets)
	h.mu.Unlock()
	if ring > 128 || latest > 32 {
		t.Fatal("event buffers unbounded", ring, latest)
	}
	snapshot := h.snapshot(0, "one")
	if !snapshot["reconcile"].(bool) || len(snapshot["events"].([]JobEvent)) > 32 {
		t.Fatal(snapshot)
	}
	if len(h.snapshot(0, "two")["events"].([]JobEvent)) != 0 {
		t.Fatal("old survey event delivered")
	}
	h.publish(JobEvent{SurveyID: "one", JobID: "job", Namespace: "another-scope"})
	h.mu.Lock()
	reconcile := h.latest.Reconcile
	h.mu.Unlock()
	if !reconcile {
		t.Fatal("cross-scope coalescing lost reconciliation marker")
	}
	close(release)
}

func TestNativeRuntimeBootstrapIsScopedToWindow(t *testing.T) {
	s := adminServer(t)
	w := adminRequest(t, s, "GET", "/map", nil, "")
	if strings.Contains(w.Body.String(), `src="/app/native.js"`) {
		t.Fatal("browser mode loaded desktop IPC")
	}
	s.o.Host = cancelledAdminHost{}
	w = adminRequest(t, s, "GET", "/map", nil, "")
	if !strings.Contains(w.Body.String(), `src="/app/native.js"`) || strings.Index(w.Body.String(), `src="/app/native.js"`) > strings.Index(w.Body.String(), `src="/app/bridge.js"`) {
		t.Fatal("extensionless native page missing ordered runtime bootstrap")
	}
}

func TestManualFitsPublishCommittedEvents(t *testing.T) {
	s := preparationServer(t)
	savePreparation(t, s, map[string]any{"paused": true})
	addPreparationRows(t, s, 2)
	w := adminRequest(t, s, "POST", "/api/app/fit-all", nil, "")
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Jobs.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	events := s.events.snapshot(0, s.surveyID)["events"].([]JobEvent)
	if len(events) != 2 {
		t.Fatal("manual job missing live commits", events)
	}
	c, put, _ := s.Atlas.Conn(nil)
	defer put()
	for _, e := range events {
		if e.JobID != s.Jobs.Snapshot()["id"] || !strings.HasPrefix(e.Namespace, "manual:") || len(e.Targets) != 1 || e.Targets[0].Version <= 0 {
			t.Fatal(e)
		}
		v, err := db.Scalar(c, "SELECT count(*) FROM estimates WHERE json_extract(result,'$.ssid')=?", e.Targets[0].SSID)
		if err != nil || db.Int(v) != 1 {
			t.Fatal("event before committed manual fit", e, v, err)
		}
	}
}

func TestSolverChangeReconcilesMapWithoutReloadingObservations(t *testing.T) {
	s := preparationServer(t)
	savePreparation(t, s, map[string]any{"paused": true})
	addPreparationRows(t, s, 2)
	get := func() map[string]any {
		w := adminRequest(t, s, "GET", "/api/app/map/summary", nil, "")
		var v map[string]any
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		return v
	}
	before := get()
	w := adminRequest(t, s, "POST", "/api/app/settings", map[string]any{"fast_fits": true}, "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	after := get()
	if before["revision"] == after["revision"] || before["observation_revision"] != after["observation_revision"] {
		t.Fatal("solver change mixed view namespaces or invalidated unchanged observations", before, after)
	}
	events := s.events.snapshot(0, s.surveyID)["events"].([]JobEvent)
	found := false
	for _, e := range events {
		if e.Kind == "solver-changed" && e.Solver == "fast" && e.Reconcile {
			found = true
		}
	}
	if !found {
		t.Fatal("missing solver reconciliation event", events)
	}
}
