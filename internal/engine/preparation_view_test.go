package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"signal-atlas/internal/atlas"
	"strings"
	"sync"
	"testing"
	"time"
)

func addViewportRows(t *testing.T, s *Server) {
	var text strings.Builder
	text.WriteString("MAC,SSID,FirstSeen,Frequency,RSSI,CurrentLatitude,CurrentLongitude,AccuracyMeters,Type\n")
	for n := 0; n < 80; n++ {
		name := fmt.Sprintf("A Far %03d", n)
		lat, lon := 49.0, -124.0
		if n >= 60 {
			name = fmt.Sprintf("Z Local %03d", n)
			lat, lon = 48.4, -123.4
		}
		for i := 0; i < 8; i++ {
			fmt.Fprintf(&text, "02:71:00:00:00:%02X,%s,2026-11-01,2412,%d,%.7f,%.7f,3,WIFI\n", n, name, -50-i, lat+float64(i%3)*.00001, lon+float64(i%2)*.00001)
		}
	}
	file := filepath.Join(t.TempDir(), "view.csv")
	if err := os.WriteFile(file, []byte(text.String()), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Atlas.ImportCSV(file, false, nil, "", nil); err != nil {
		t.Fatal(err)
	}
}
func TestViewportPriorityRunsLocalFirstAndKeepsGlobalProgress(t *testing.T) {
	s := preparationServer(t)
	savePreparation(t, s, map[string]any{"paused": true, "workers": 1})
	addViewportRows(t, s)
	var mu sync.Mutex
	var order []string
	s.preparation.mu.Lock()
	s.preparation.prepare = func(_ context.Context, _ atlas.PreparationNamespace, target atlas.PreparationTarget, _ int64) (atlas.PreparedTarget, error) {
		mu.Lock()
		order = append(order, target.SSID)
		mu.Unlock()
		return atlas.PreparedTarget{Target: target, Result: atlas.PreparedTargetResult{Unavailable: 1}}, nil
	}
	s.preparation.mu.Unlock()
	r := adminRequest(t, s, "GET", "/api/app/map/features?bbox=-123.401,48.399,-123.399,48.401&zoom=16&clustering=1", nil, "")
	if r.Code != 200 {
		t.Fatal(r.Body.String())
	}
	savePreparation(t, s, map[string]any{"paused": false})
	waitPreparation(t, s, "complete")
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 80 {
		t.Fatal("lost/duplicated work", len(order))
	}
	for i := 0; i < 3; i++ {
		if !strings.HasPrefix(order[i], "Z Local") {
			t.Fatal("view not first", order[:4])
		}
	}
	if !strings.HasPrefix(order[3], "A Far") {
		t.Fatal("global preparation starved", order[:4])
	}
	local := 0
	for _, name := range order[:27] {
		if strings.HasPrefix(name, "Z Local") {
			local++
		}
	}
	if local != 20 {
		t.Fatal("local work waited behind global queue", local)
	}
}
func TestPanReprioritizesQueuedWorkWithoutCancellingRunningFit(t *testing.T) {
	s := preparationServer(t)
	savePreparation(t, s, map[string]any{"paused": true, "workers": 1})
	addViewportRows(t, s)
	first, release, second := make(chan struct{}), make(chan struct{}), make(chan string, 1)
	var once sync.Once
	calls := 0
	s.preparation.mu.Lock()
	s.preparation.prepare = func(ctx context.Context, _ atlas.PreparationNamespace, target atlas.PreparationTarget, _ int64) (atlas.PreparedTarget, error) {
		calls++
		if calls == 1 {
			once.Do(func() { close(first) })
			select {
			case <-release:
			case <-ctx.Done():
				return atlas.PreparedTarget{}, ctx.Err()
			}
		} else if calls == 2 {
			second <- target.SSID
		}
		return atlas.PreparedTarget{Target: target, Result: atlas.PreparedTargetResult{Unavailable: 1}}, nil
	}
	s.preparation.mu.Unlock()
	savePreparation(t, s, map[string]any{"paused": false})
	select {
	case <-first:
	case <-time.After(3 * time.Second):
		t.Fatal("no calculation")
	}
	r := adminRequest(t, s, "GET", "/api/app/map/features?bbox=-123.401,48.399,-123.399,48.401&zoom=16&clustering=1", nil, "")
	if r.Code != 200 {
		t.Fatal(r.Body.String())
	}
	// Let the coordinator see the new view while the original fit remains active.
	time.Sleep(50 * time.Millisecond)
	close(release)
	select {
	case name := <-second:
		if !strings.HasPrefix(name, "Z Local") {
			t.Fatal("pan did not reprioritize next fit", name)
		}
	case <-time.After(time.Second):
		t.Fatal("no local follow-up")
	}
	waitPreparation(t, s, "complete")
}

func TestViewportRejectsLatePanAndIgnoresDuplicateRefresh(t *testing.T) {
	p := &preparationRunner{wake: make(chan struct{}, 1)}
	a := [4]float64{-124, 48, -123, 49}
	b := [4]float64{-123, 48, -122, 49}
	p.setViewportRequest(b, "map", 2)
	_, revision := p.viewport()
	p.setViewportRequest(a, "map", 1)
	got, after := p.viewport()
	if got != b || after != revision {
		t.Fatal("late pan replaced current view")
	}
	p.setViewportRequest(b, "map", 3)
	_, after = p.viewport()
	if after != revision {
		t.Fatal("live refresh restarted spatial scan")
	}
	p.setViewportRequest(a, "new map", 1)
	got, _ = p.viewport()
	if got != a {
		t.Fatal("new map session could not prioritize")
	}
}
