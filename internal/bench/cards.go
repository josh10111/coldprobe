package bench

import "github.com/josh10111/coldprobe/internal/runpod"

// CardPool finds cardID in the catalog and returns it with its pool.
func CardPool(catalog []runpod.Pool, cardID string) (pool runpod.Pool, card runpod.Card, ok bool) {
	for _, p := range catalog {
		for _, c := range p.Cards {
			if c.ID == cardID {
				return p, c, true
			}
		}
	}
	return runpod.Pool{}, runpod.Card{}, false
}

// ExcludedFor lists every card in pool except cardID, for the endpoint's
// gpu.excludedTypes. Never nil, so it encodes as []. A GPU Runpod adds to
// the pool after the catalog read is not excluded.
func ExcludedFor(pool runpod.Pool, cardID string) []string {
	out := []string{}
	for _, c := range pool.Cards {
		if c.ID != cardID {
			out = append(out, c.ID)
		}
	}
	return out
}

func findPool(catalog []runpod.Pool, id string) (runpod.Pool, bool) {
	for _, p := range catalog {
		if p.ID == id {
			return p, true
		}
	}
	return runpod.Pool{}, false
}
