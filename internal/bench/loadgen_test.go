package bench

import (
	"context"
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 100 rps for 200 ms is 20 sends even though each takes 50 ms.
func TestOpenLoop_KeepsScheduleWhenSlow(t *testing.T) {
	var started, finished atomic.Int64
	begin := time.Now()
	OpenLoop(context.Background(), 100, 200*time.Millisecond, func(ctx context.Context, intended time.Time) {
		started.Add(1)
		time.Sleep(50 * time.Millisecond)
		finished.Add(1)
	})
	took := time.Since(begin)

	if n := started.Load(); n < 19 || n > 21 {
		t.Errorf("send called %d times, want 19-21", n)
	}
	// Last send at ~190 ms + 50 ms of work.
	if took > 290*time.Millisecond {
		t.Errorf("OpenLoop took %v, want <= 290ms", took)
	}
	if finished.Load() != started.Load() {
		t.Errorf("returned with %d of %d sends still running", started.Load()-finished.Load(), started.Load())
	}
}

func TestOpenLoop_IntendedTimesEvenlySpaced(t *testing.T) {
	var mu sync.Mutex
	var intended []time.Time
	OpenLoop(context.Background(), 100, 100*time.Millisecond, func(ctx context.Context, at time.Time) {
		mu.Lock()
		intended = append(intended, at)
		mu.Unlock()
	})

	if len(intended) < 2 {
		t.Fatalf("got %d sends, want at least 2", len(intended))
	}
	slices.SortFunc(intended, func(a, b time.Time) int { return a.Compare(b) })
	for i := 1; i < len(intended); i++ {
		gap := intended[i].Sub(intended[i-1])
		if gap != 10*time.Millisecond {
			t.Errorf("gap %d = %v, want exactly 10ms", i, gap)
		}
	}
}

func TestOpenLoop_StopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	var calls atomic.Int64
	begin := time.Now()
	OpenLoop(ctx, 100, 10*time.Second, func(ctx context.Context, intended time.Time) {
		calls.Add(1)
		<-ctx.Done() // a send that only ends when cancelled
	})
	took := time.Since(begin)

	if took > 200*time.Millisecond {
		t.Errorf("OpenLoop took %v after a 50ms cancel, want < 200ms", took)
	}
	if n := calls.Load(); n < 1 || n > 7 {
		t.Errorf("send called %d times in ~50ms at 100 rps, want 1-7", n)
	}
}

func TestOpenLoop_AlreadyCancelledSendsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var calls atomic.Int64
	OpenLoop(ctx, 100, time.Second, func(ctx context.Context, intended time.Time) { calls.Add(1) })
	if n := calls.Load(); n != 0 {
		t.Errorf("send called %d times on a cancelled ctx, want 0", n)
	}
}

func TestOpenLoop_ZeroRateReturns(t *testing.T) {
	var calls atomic.Int64
	OpenLoop(context.Background(), 0, time.Second, func(ctx context.Context, intended time.Time) { calls.Add(1) })
	if n := calls.Load(); n != 0 {
		t.Errorf("send called %d times at 0 rps, want 0", n)
	}
}

// At +Inf rps the interval rounds to 0; the ctx timeout only bounds a regression.
func TestOpenLoop_ZeroIntervalReturns(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	var calls atomic.Int64
	OpenLoop(ctx, math.Inf(1), time.Second, func(ctx context.Context, intended time.Time) { calls.Add(1) })
	if n := calls.Load(); n != 0 {
		t.Errorf("send called %d times at +Inf rps, want 0", n)
	}
}
