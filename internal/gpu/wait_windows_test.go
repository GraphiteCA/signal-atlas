//go:build windows && amd64

package gpu

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestQueuedGPURequestCancelsWithoutDriverCalls(t *testing.T) {
	d := &clDevice{gate: make(chan struct{}, 1)}
	d.gate <- struct{}{}
	var cancelled atomic.Bool
	var polls atomic.Int32
	queued := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, e := d.score(Input{}, func() bool {
			if polls.Add(1) == 2 {
				close(queued)
			}
			return cancelled.Load()
		})
		done <- e
	}()
	select {
	case <-queued:
	case <-time.After(time.Second):
		t.Fatal("GPU request did not enter queue wait")
	}
	cancelled.Store(true)
	select {
	case e := <-done:
		if !errors.Is(e, ErrCancelled) {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		t.Fatal("queued GPU call ignored cancellation")
	}
}
