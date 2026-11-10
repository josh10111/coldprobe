//go:build integration

// Read-only smoke test against the real API; skips without RUNPOD_API_KEY.
//
//	go test -tags integration ./internal/runpod/

package runpod

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestLive_ReadOnly(t *testing.T) {
	key := os.Getenv("RUNPOD_API_KEY")
	if key == "" {
		t.Skip("RUNPOD_API_KEY not set")
	}
	m, err := NewMgmt("", key)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pools, err := m.Pools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pools) == 0 {
		t.Fatal("want at least 1 serverless pool")
	}
	for _, p := range pools {
		t.Logf("pool %s: $%.2f/h, %d GB, %v", p.ID, p.PricePerHr, p.MemoryGB, p.GPUNames)
	}

	// Endpoint names and IDs are account data: log only the count.
	eps, err := m.ListEndpoints(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d endpoints on the account", len(eps))
}
