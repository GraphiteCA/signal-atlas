package gpu

import (
	"errors"
	"math"
	"testing"
)

func TestUnsafeInputsFailBeforeDriverInitialization(t *testing.T) {
	valid := func() Input {
		return Input{X: []float64{1}, Y: []float64{2}, RSSI: []float64{-50}, Weights: []float64{1}, WeightSums: []float64{1}, Groups: []int32{0}, CandidateX: []float64{0}, CandidateY: []float64{0}, Floor: 3, Factor: 30}
	}
	for _, change := range []func(*Input){func(v *Input) { v.Groups[0] = -1 }, func(v *Input) { v.Groups[0] = 1 }, func(v *Input) { v.X[0] = math.NaN() }, func(v *Input) { v.Weights[0] = 0 }, func(v *Input) { v.WeightSums[0] = 0 }, func(v *Input) { v.Floor = 0 }, func(v *Input) { v.CandidateY = nil }} {
		in := valid()
		change(&in)
		if _, e := Score(in, nil); !errors.Is(e, ErrUnsupported) {
			t.Fatal(e)
		}
	}
	if _, e := Score(valid(), func() bool { return true }); !errors.Is(e, ErrCancelled) {
		t.Fatal(e)
	}
}
