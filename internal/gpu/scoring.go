// Package gpu supplies optional bounded float64 OpenCL scoring. CPU fitting,
// grouping, sampling, refinement and durable writes remain in Atlas.
package gpu

import (
	"errors"
	"math"
	"sync"
	"sync/atomic"
)

var ErrCancelled = errors.New("GPU scoring cancelled")
var ErrUnsupported = errors.New("GPU input exceeds supported bounds")

type Input struct {
	X, Y, RSSI, Weights, WeightSums []float64
	Groups                          []int32
	CandidateX, CandidateY          []float64
	Floor, Factor, Within           float64
}
type device interface {
	score(Input, func() bool) ([]float64, error)
	close()
}

var manager struct {
	once             sync.Once
	mu               sync.Mutex
	device           device
	name, reason     string
	checked          bool
	calls, fallbacks atomic.Int64
}

func Probe() map[string]any {
	manager.once.Do(func() {
		d, name, e := open()
		manager.mu.Lock()
		defer manager.mu.Unlock()
		manager.checked = true
		manager.device = d
		manager.name = name
		if e != nil {
			manager.reason = e.Error()
		}
	})
	return Status()
}
func Status() map[string]any {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return map[string]any{"checked": manager.checked, "available": manager.device != nil && manager.reason == "", "device": manager.name, "reason": manager.reason, "calls": manager.calls.Load(), "fallbacks": manager.fallbacks.Load(), "precision": "float64", "scratch_limit_mb": 64}
}
func Disable(reason string) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.reason = reason
}
func Score(in Input, cancelled func() bool) ([]float64, error) {
	if cancelled != nil && cancelled() {
		return nil, ErrCancelled
	}
	if !supported(in) {
		manager.fallbacks.Add(1)
		return nil, ErrUnsupported
	}
	Probe()
	manager.mu.Lock()
	d, reason := manager.device, manager.reason
	manager.mu.Unlock()
	if d == nil || reason != "" {
		manager.fallbacks.Add(1)
		return nil, errors.New("GPU unavailable: " + reason)
	}
	v, e := d.score(in, cancelled)
	if e != nil {
		if !errors.Is(e, ErrCancelled) {
			manager.fallbacks.Add(1)
			Disable(e.Error())
		}
		return nil, e
	}
	manager.calls.Add(1)
	return v, nil
}

func supported(in Input) bool {
	n, p := len(in.X), len(in.CandidateX)
	if n < 1 || n > 4096 || p < 1 || p > 16384 || len(in.Y) != n || len(in.RSSI) != n || len(in.Weights) != n || len(in.Groups) != n || len(in.CandidateY) != p || len(in.WeightSums) < 1 || len(in.WeightSums) > n {
		return false
	}
	for _, a := range [][]float64{in.X, in.Y, in.RSSI, in.Weights, in.WeightSums, in.CandidateX, in.CandidateY, {in.Floor, in.Factor, in.Within}} {
		for _, v := range a {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return false
			}
		}
	}
	if in.Floor <= 0 || in.Within < 0 {
		return false
	}
	for i, g := range in.Groups {
		if g < 0 || int(g) >= len(in.WeightSums) || in.Weights[i] <= 0 {
			return false
		}
	}
	for _, w := range in.WeightSums {
		if w <= 0 {
			return false
		}
	}
	return true
}
