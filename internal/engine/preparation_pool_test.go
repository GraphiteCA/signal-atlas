package engine

import (
	"context"
	"signal-atlas/internal/atlas"
	"sync"
	"sync/atomic"
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
	if !ok || batches < 1 || batches > 5 {
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

func TestFrequentClusteredReadsDoNotStarvePreparation(t *testing.T) {
	s := preparationServer(t)
	addPreparationRows(t, s, 40)
	s.preparation.mu.Lock()
	s.preparation.prepare = func(ctx context.Context, _ atlas.PreparationNamespace, target atlas.PreparationTarget, _ int64) (atlas.PreparedTarget, error) {
		select {
		case <-time.After(5 * time.Millisecond):
		case <-ctx.Done():
			return atlas.PreparedTarget{}, ctx.Err()
		}
		return atlas.PreparedTarget{Target: target, Result: atlas.PreparedTargetResult{Unavailable: 1}}, nil
	}
	s.preparation.mu.Unlock()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			w := adminRequest(t, s, "GET", "/api/app/map/features?clustering=1&zoom=12&bbox=-124,48,-123,49", nil, "")
			if w.Code != 200 {
				t.Errorf("map: %d", w.Code)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	defer func() { close(stop); <-done }()
	// The previous post-map 100 ms cooldown never expired under this workload.
	state := waitPreparation(t, s, "complete")
	if state["progress"].(map[string]any)["completed"] != float64(40) {
		t.Fatal(state)
	}
}

// A blocked database writer must not stop the next batch's calculations, and
// it must not allow an unbounded queue of computed results to accumulate.
func TestPreparationOverlapsWriterWithOneBoundedCollectingBatch(t *testing.T) {
	s := preparationServer(t)
	savePreparation(t, s, map[string]any{"paused": true, "workers": 2})
	addPreparationRows(t, s, 1200)
	writing, release := make(chan int, 1), make(chan struct{})
	var first sync.Once
	var calculated atomic.Int64
	s.preparation.mu.Lock()
	s.preparation.prepare = func(_ context.Context, _ atlas.PreparationNamespace, target atlas.PreparationTarget, _ int64) (atlas.PreparedTarget, error) {
		calculated.Add(1)
		return atlas.PreparedTarget{Target: target, Result: atlas.PreparedTargetResult{Unavailable: 1}}, nil
	}
	s.preparation.commit = func(ctx context.Context, ns atlas.PreparationNamespace, batch []atlas.PreparedTarget) ([]atlas.PreparedTarget, error) {
		first.Do(func() { writing <- len(batch) })
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return s.Atlas.CommitPreparationBatch(ctx, ns, batch)
	}
	s.preparation.mu.Unlock()
	savePreparation(t, s, map[string]any{"paused": false})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	var writingCount int
	select {
	case writingCount = <-writing:
	case <-time.After(3 * time.Second):
		t.Fatal("writer never started")
	}
	// The timer can submit a partial first batch under the race detector. Test
	// overlap relative to its actual size, rather than assuming exactly 512.
	deadline := time.Now().Add(3 * time.Second)
	for calculated.Load() < int64(writingCount+512) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if calculated.Load() < int64(writingCount+512) {
		t.Fatal("calculations stalled behind the writer", writingCount, calculated.Load())
	}
	time.Sleep(50 * time.Millisecond)
	if n := calculated.Load(); n > int64(writingCount+518) {
		t.Fatal("computed results grew beyond the bounded pipeline", n)
	}
	unblock()
	state := waitPreparation(t, s, "complete")
	if calculated.Load() != 1200 || state["progress"].(map[string]any)["completed"] != float64(1200) {
		t.Fatal("lost or duplicate work", calculated.Load(), state)
	}
}
