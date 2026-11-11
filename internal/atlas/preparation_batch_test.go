package atlas

import (
	"context"
	"math"
	"math/rand/v2"
	"reflect"
	"signal-atlas/internal/db"
	"sort"
	"testing"
)

func TestPreparationBatchSkipsStaleAndPublishesAtomicCheckpoints(t *testing.T) {
	a := preparationFixture(t)
	ns := preparationNamespace(t, a, "sample", "shared", true)
	ctx := context.Background()
	targets, e := a.NextPreparationTargets(ctx, ns.ID, 256<<20, 3)
	if e != nil {
		t.Fatal(e)
	}
	batch := []PreparedTarget{}
	for _, target := range targets {
		p, e := a.ComputePreparationTarget(ctx, ns, target, 256<<20)
		if e != nil {
			t.Fatal(e)
		}
		batch = append(batch, p)
	}
	c, put, _ := a.Conn(ctx)
	defer put()
	db.Exec(c, "UPDATE preparation_targets SET version=version+1 WHERE target_key=?", targets[0].Key)
	accepted, e := a.CommitPreparationBatch(ctx, ns, batch)
	if e != nil || len(accepted) != 2 {
		t.Fatal(accepted, e)
	}
	for _, target := range targets {
		v, e := db.Scalar(c, "SELECT version FROM preparation_checkpoints WHERE namespace=? AND target_key=?", ns.ID, target.Key)
		if e != nil {
			t.Fatal(e)
		}
		if target.Key == targets[0].Key {
			if v != nil {
				t.Fatal("stale checkpoint published")
			}
		} else if db.Int(v) != target.Version {
			t.Fatal(v, target)
		}
	}
	again, e := a.CommitPreparationBatch(ctx, ns, batch[1:])
	if e != nil || len(again) != 0 {
		t.Fatal("checkpoint replay", again, e)
	}
}
func TestPreparationBatchRollsBackAllResultsOnFailure(t *testing.T) {
	a := preparationFixture(t)
	ns := preparationNamespace(t, a, "sample", "shared", true)
	ctx := context.Background()
	targets, _ := a.NextPreparationTargets(ctx, ns.ID, 256<<20, 3)
	batch := []PreparedTarget{}
	for _, target := range targets {
		p, e := a.ComputePreparationTarget(ctx, ns, target, 256<<20)
		if e != nil {
			t.Fatal(e)
		}
		batch = append(batch, p)
	}
	// The staging JSON validity constraint rejects malformed results; neither
	// earlier estimates nor their checkpoints may survive.
	batch[len(batch)-1].Writes = []PreparedEstimate{{Key: "invalid-json", Text: "{"}}
	if _, e := a.CommitPreparationBatch(ctx, ns, batch); e == nil {
		t.Fatal("bad result accepted")
	}
	if n := len(table(t, a, "SELECT * FROM preparation_checkpoints")); n != 0 {
		t.Fatal("partial checkpoints", n)
	}
	if n := len(table(t, a, "SELECT * FROM estimates")); n != 0 {
		t.Fatal("partial estimates", n)
	}
	if _, e := a.CommitPreparationBatch(ctx, ns, batch[:1]); e != nil {
		t.Fatal("failed transaction poisoned pool", e)
	}
}
func TestCoarseHeapMatchesStableReferenceIncludingTies(t *testing.T) {
	rng := rand.New(rand.NewPCG(14, 53))
	for trial := 0; trial < 50; trial++ {
		x, y, scores := make([]float64, 1000), make([]float64, 1000), make([]float64, 1000)
		indices := make([]int, 1000)
		for i := range x {
			x[i] = float64(i % 40)
			y[i] = float64(i / 40)
			scores[i] = float64(rng.IntN(20))
			if trial%2 == 1 && i%37 == 0 {
				scores[i] = math.NaN()
			}
			indices[i] = i
		}
		sort.SliceStable(indices, func(i, j int) bool { return scores[indices[i]] < scores[indices[j]] })
		expected := [][2]float64{}
		for _, j := range indices {
			ok := true
			for _, st := range expected {
				dx, dy := x[j]-st[0], y[j]-st[1]
				if !(math.Sqrt(dx*dx+dy*dy) > 3) {
					ok = false
					break
				}
			}
			if ok {
				expected = append(expected, [2]float64{x[j], y[j]})
				if len(expected) == 8 {
					break
				}
			}
		}
		actual, e := coarseStarts(x, y, scores, 3, nil)
		if e != nil || !reflect.DeepEqual(expected, actual) {
			t.Fatal(expected, actual, e)
		}
	}
}
