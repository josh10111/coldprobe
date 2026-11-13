package bench

import (
	"context"
	"math"
	"sync"
	"time"
)

// OpenLoop calls send rps times per second for dur on a fixed schedule and
// returns once every send has returned.
//
// Open loop avoids coordinated omission: send i is due at start+i/rps
// however slow earlier sends are, and the caller measures latency from
// intended, so lateness counts against the endpoint.
func OpenLoop(ctx context.Context, rps float64, dur time.Duration, send func(ctx context.Context, intended time.Time)) {
	// !(rps > 0) also rejects NaN.
	if !(rps > 0) || dur <= 0 {
		return
	}
	// Each send's offset is computed from i, not as i*interval: a truncated
	// interval (1/3 s) drifts early and squeezes in one send too many.
	offset := func(i int) time.Duration {
		return time.Duration(math.Round(float64(i) * float64(time.Second) / rps))
	}
	// A rate above 1e9/s makes the first step 0 and the schedule would never
	// advance.
	if offset(1) <= 0 {
		return
	}

	var wg sync.WaitGroup
	start := time.Now()
	end := start.Add(dur)

	for i := 0; ; i++ {
		intended := start.Add(offset(i))
		if !intended.Before(end) {
			break
		}
		if !waitUntil(ctx, intended) {
			break
		}
		wg.Go(func() { send(ctx, intended) })
	}
	wg.Wait()
}

// waitUntil blocks until t or ctx is done, and reports whether to send.
func waitUntil(ctx context.Context, t time.Time) bool {
	timer := time.NewTimer(time.Until(t))
	defer timer.Stop()
	select {
	case <-timer.C:
		// select picks at random when both are ready.
		return ctx.Err() == nil
	case <-ctx.Done():
		return false
	}
}
