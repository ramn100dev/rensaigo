package safe

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestCallAndGo(t *testing.T) {
	if err := Call("x", func() error { var m map[string]int; m["a"] = 1; return nil }); err == nil {
		t.Error("el pánico debería volver como error")
	}
	done := make(chan struct{})
	Go("y", func() { defer close(done); panic("boom") })
	<-done // si el pánico escapara, el test entero se caería
}

func TestLoopRestarts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var n atomic.Int32
	go Loop(ctx, "z", func(ctx context.Context) {
		if n.Add(1) == 1 {
			panic("primera vuelta")
		}
		<-ctx.Done()
	})
	time.Sleep(50 * time.Millisecond)
	cancel()
	if n.Load() != 1 {
		t.Fatalf("vueltas: %d", n.Load())
	}
}
