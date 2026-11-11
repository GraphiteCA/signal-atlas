package engine

import (
	"context"
	"signal-atlas/internal/atlas"
	"sync"
	"testing"
	"time"
)

func TestPreparationPoolDoesNotWaitForSlowWave(t *testing.T) {
	s := preparationServer(t)
	addPreparationRows(t, s, 4)
	slow := make(chan struct{})
	third := make(chan struct{})
	release := make(chan struct{})
	var once, thirdOnce sync.Once
	s.preparation.mu.Lock()
	s.preparation.prepare = func(ctx context.Context, _ atlas.PreparationNamespace, target atlas.PreparationTarget, _ int64) (atlas.PreparedTarget, error) {
		if target.SSID == "Auto0" {
			once.Do(func() { close(slow) })
			select {
			case <-release:
			case <-ctx.Done():
				return atlas.PreparedTarget{}, ctx.Err()
			}
		}
		if target.SSID == "Auto2" {
			thirdOnce.Do(func() { close(third) })
		}
		return atlas.PreparedTarget{Target: target, Result: atlas.PreparedTargetResult{Unavailable: 1}}, nil
	}
	s.preparation.mu.Unlock()
	savePreparation(t, s, map[string]any{"workers": 2})
	select {
	case <-slow:
	case <-time.After(3 * time.Second):
		t.Fatal("slow worker never started")
	}
	select {
	case <-third:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("free worker waited for the slow target")
	}
	close(release)
	waitPreparation(t, s, "complete")
}
func TestPreparationPoolBatchesFastCompletions(t *testing.T) {
	s := preparationServer(t)
	addPreparationRows(t, s, 150)
	s.preparation.mu.Lock()
	s.preparation.prepare = func(_ context.Context, _ atlas.PreparationNamespace, target atlas.PreparationTarget, _ int64) (atlas.PreparedTarget, error) {
		return atlas.PreparedTarget{Target: target, Result: atlas.PreparedTargetResult{Unavailable: 1}}, nil
	}
	s.preparation.mu.Unlock()
	status := waitPreparation(t, s, "complete")
	batches, ok := status["commit_batches"].(float64)
	if !ok || batches < 3 || batches > 5 {
		t.Fatal("per-target commits were not batched", status)
	}
	if status["progress"].(map[string]any)["completed"] != float64(150) {
		t.Fatal(status)
	}
}

func TestClusteredMapRetainsInflightPreparation(t *testing.T) {
	s := preparationServer(t)
	addPreparationRows(t, s, 1)
	started, release := make(chan struct{}), make(chan struct{})
	cancelled := make(chan struct{}, 1)
	var once sync.Once
	s.preparation.mu.Lock()
	s.preparation.prepare = func(ctx context.Context, _ atlas.PreparationNamespace, target atlas.PreparationTarget, _ int64) (atlas.PreparedTarget, error) {
		once.Do(func() { close(started) })
		select {
		case <-ctx.Done():
			cancelled <- struct{}{}
			return atlas.PreparedTarget{}, ctx.Err()
		case <-release:
			return atlas.PreparedTarget{Target: target, Result: atlas.PreparedTargetResult{Unavailable: 1}}, nil
		}
	}
	s.preparation.mu.Unlock()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("background work did not start")
	}
	w := adminRequest(t, s, "GET", "/api/app/map/features?clustering=1&zoom=15&bbox=-124,48,-123,49", nil, "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	select {
	case <-cancelled:
		t.Fatal("clustered map discarded an in-flight calculation")
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	state := waitPreparation(t, s, "complete")
	if state["progress"].(map[string]any)["completed"] != float64(1) {
		t.Fatal(state)
	}
}
