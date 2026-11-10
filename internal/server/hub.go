package server

import (
	"sync"

	"github.com/josh10111/coldprobe/internal/bench"
)

// subBuffer is how far a subscriber can fall behind before its events drop.
const subBuffer = 256

// Hub fans the Runner's events out to SSE subscribers, per run.
//
// Publish never blocks the Runner: a subscriber with a full buffer loses the
// event, and the UI detects the Seq gap and re-fetches the run snapshot.
type Hub struct {
	mu   sync.Mutex
	seq  map[string]int64                         // run ID -> last Seq handed out
	subs map[string]map[chan bench.Event]struct{} // run ID -> set of subscriber channels
}

// NewHub returns an empty hub.
func NewHub() *Hub {
	return &Hub{
		seq:  map[string]int64{},
		subs: map[string]map[chan bench.Event]struct{}{},
	}
}

// Publish numbers e per run and sends it to every subscriber of e.RunID.
// Numbering and sending share one lock so subscribers see Seq order.
func (h *Hub) Publish(e bench.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.seq[e.RunID]++
	e.Seq = h.seq[e.RunID]
	if isFinal(e) {
		delete(h.seq, e.RunID)
	}

	for ch := range h.subs[e.RunID] {
		select {
		case ch <- e:
		default: // buffer full: drop for this subscriber
		}
	}
}

// Subscribe returns a channel of runID's events and an unsubscribe func.
// The channel is never closed, since a concurrent Publish could send on it.
func (h *Hub) Subscribe(runID string) (<-chan bench.Event, func()) {
	ch := make(chan bench.Event, subBuffer)

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subs[runID] == nil {
		h.subs[runID] = map[chan bench.Event]struct{}{}
	}
	h.subs[runID][ch] = struct{}{}

	unsubscribe := func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		delete(h.subs[runID], ch)
		if len(h.subs[runID]) == 0 {
			delete(h.subs, runID)
		}
	}
	return ch, unsubscribe
}

// isFinal reports whether e is a run's last event: a non-"running" run_status.
func isFinal(e bench.Event) bool {
	if e.Type != "run_status" {
		return false
	}
	m, ok := e.Data.(map[string]any)
	return ok && m["status"] != "running"
}
