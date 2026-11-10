package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/josh10111/coldprobe/internal/fakerunpod"
	"github.com/josh10111/coldprobe/internal/runpod"
	"github.com/josh10111/coldprobe/internal/store"
)

func TestLoggerRedactsKey(t *testing.T) {
	const key = "secret-key-123" // a made-up key, not a real one
	var buf bytes.Buffer
	log := newLogger(&buf, key)

	log.Info("calling with "+key,
		slog.String("x", "Bearer secret-key-123"),
		slog.Any("err", errors.New("runpod: HTTP 401: key secret-key-123 rejected")),
		slog.Group("req", slog.String("auth", key)), // inside a group too
	)

	out := buf.String()
	if strings.Contains(out, key) {
		t.Fatalf("key leaked into the log: %s", out)
	}
	for _, want := range []string{`"x":"Bearer [REDACTED]"`, `"err":"runpod: HTTP 401: key [REDACTED] rejected"`, `"auth":"[REDACTED]"`, `calling with [REDACTED]`} {
		if !strings.Contains(out, want) {
			t.Errorf("log %s lacks %s", out, want)
		}
	}
}

func TestStartup_MarksCrashedAndRunsJanitor(t *testing.T) {
	fake := fakerunpod.New(fakerunpod.TestOptions())
	t.Cleanup(fake.Close)
	st, err := store.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	mgmt, err := runpod.NewMgmt(fake.MgmtURL(), "test-key")
	if err != nil {
		t.Fatal(err)
	}

	ctx := t.Context()
	// A run a previous process left "running" when it died.
	if err := st.CreateRun(ctx, store.Run{ID: "r1", CreatedAt: time.Now(), Status: "running", Params: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	fake.AddEndpoint("bench-coldprobe-x") // its leftover endpoint
	keep := fake.AddEndpoint("keep-me")   // not ours: must survive
	// Not in our database and a minute old: maybe another instance's.
	young := fake.AddEndpoint("bench-coldprobe-" + time.Now().UTC().Add(-time.Minute).Format("20060102-150405") + "-ffff-0")

	if err := startup(ctx, st, mgmt, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}

	run, err := st.GetRun(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "aborted_crash" {
		t.Errorf("run status = %q, want aborted_crash", run.Status)
	}
	if live := fake.LiveEndpoints(); len(live) != 2 || live[0] != keep || live[1] != young {
		t.Errorf("live endpoints = %v, want only keep-me (%s) and the young unknown one (%s)", live, keep, young)
	}
}

// flock binds to the open file, so a second lockDB in this process
// conflicts like a second process would.
func TestLockDB_SecondHolderIsRefused(t *testing.T) {
	db := filepath.Join(t.TempDir(), "c.db")
	first, err := lockDB(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockDB(db); err == nil || err.Error() != "another coldprobe is already using "+db {
		t.Errorf("second lockDB = %v, want \"another coldprobe is already using %s\"", err, db)
	}
	first.Close()
	third, err := lockDB(db)
	if err != nil {
		t.Fatalf("lockDB after the first holder closed = %v, want the lock", err)
	}
	third.Close()
}

// COLDPROBE_FAKE_NOSTOCK: a pool id sets every card in it, a card id just
// that card, and unknown ids or levels are errors.
func TestApplyNoStock(t *testing.T) {
	stock := func(t *testing.T, value string) map[string]string {
		t.Helper()
		fake := fakerunpod.New(fakerunpod.TestOptions())
		t.Cleanup(fake.Close)
		if err := applyNoStock(fake, fakerunpod.DefaultPools(), value); err != nil {
			t.Fatal(err)
		}
		mgmt, err := runpod.NewMgmt(fake.MgmtURL(), "test-key")
		if err != nil {
			t.Fatal(err)
		}
		pools, err := mgmt.Pools(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, p := range pools {
			out[p.ID] = p.Availability
			for _, c := range p.Cards {
				out[c.ID] = c.Availability
			}
		}
		return out
	}
	for value, want := range map[string]map[string]string{
		"AMPERE_24":                          {"AMPERE_24": "NONE", "NVIDIA RTX A5000": "NONE", "NVIDIA L4": "NONE", "AMPERE_16": "HIGH"},
		"AMPERE_24:LOW":                      {"AMPERE_24": "LOW", "NVIDIA L4": "LOW", "AMPERE_16": "HIGH"},
		"NVIDIA RTX 2000 Ada Generation":     {"NVIDIA RTX 2000 Ada Generation": "NONE", "NVIDIA RTX A4000": "MEDIUM", "AMPERE_16": "MEDIUM"},
		"NVIDIA RTX 2000 Ada Generation:LOW": {"NVIDIA RTX 2000 Ada Generation": "LOW", "AMPERE_16": "MEDIUM"},
		"":                                   {"AMPERE_16": "HIGH", "AMPERE_24": "HIGH"},
	} {
		got := stock(t, value)
		for id, level := range want {
			if got[id] != level {
				t.Errorf("COLDPROBE_FAKE_NOSTOCK=%q: %s = %q, want %q", value, id, got[id], level)
			}
		}
	}

	fake := fakerunpod.New(fakerunpod.TestOptions())
	t.Cleanup(fake.Close)
	for _, bad := range []string{"H100_80", "AMPERE_24:EMPTY"} {
		if err := applyNoStock(fake, fakerunpod.DefaultPools(), bad); err == nil {
			t.Errorf("COLDPROBE_FAKE_NOSTOCK=%q: no error", bad)
		}
	}
}

// getsWorker reports whether a job on a new endpoint in pool gets a
// worker on fake within 200 ms (the fake boots one in ~50 ms).
func getsWorker(t *testing.T, fake *fakerunpod.Fake, pool string) bool {
	t.Helper()
	mgmt, err := runpod.NewMgmt(fake.MgmtURL(), "test-key")
	if err != nil {
		t.Fatal(err)
	}
	id, err := mgmt.CreateEndpoint(t.Context(), runpod.EndpointSpec{
		Name: "bench-coldprobe-test-place", Image: "img", Pool: pool, FlashBoot: "OFF", MaxWorkers: 1, IdleTimeoutS: 5})
	if err != nil {
		t.Fatal(err)
	}
	jobs := runpod.NewJobClient(fake.JobURL(), "test-key", nil)
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	res, _ := jobs.RunSync(ctx, id, map[string]any{"sleep_ms": 1}, 100*time.Millisecond)
	return res.Status == "COMPLETED"
}

// LOW still places workers, so "start anyway" runs normally.
func TestApplyNoStock_NonePoolPlacesNoWorker(t *testing.T) {
	for value, want := range map[string]bool{"AMPERE_24": false, "AMPERE_24:LOW": true} {
		fake := fakerunpod.New(fakerunpod.TestOptions())
		t.Cleanup(fake.Close)
		if err := applyNoStock(fake, fakerunpod.DefaultPools(), value); err != nil {
			t.Fatal(err)
		}
		if got := getsWorker(t, fake, "AMPERE_24"); got != want {
			t.Errorf("COLDPROBE_FAKE_NOSTOCK=%q: job got a worker = %v, want %v", value, got, want)
		}
	}
}

// Only pool ids are accepted: placement is per pool.
func TestApplyNoPlace(t *testing.T) {
	fake := fakerunpod.New(fakerunpod.TestOptions())
	t.Cleanup(fake.Close)
	if err := applyNoPlace(fake, fakerunpod.DefaultPools(), "AMPERE_24"); err != nil {
		t.Fatal(err)
	}
	if getsWorker(t, fake, "AMPERE_24") {
		t.Error("AMPERE_24 got a worker, want none")
	}
	if !getsWorker(t, fake, "AMPERE_16") {
		t.Error("AMPERE_16 got no worker, want one")
	}
	for _, bad := range []string{"H100_80", "NVIDIA L4"} {
		if err := applyNoPlace(fake, fakerunpod.DefaultPools(), bad); err == nil {
			t.Errorf("COLDPROBE_FAKE_NOPLACE=%q: no error", bad)
		}
	}
}

// Bad values exit naming the variable but not echoing the value.
func TestLoadConfig_NoWorkerWarn(t *testing.T) {
	t.Setenv("COLDPROBE_FAKE", "1")
	for v, want := range map[string]time.Duration{"1": time.Second, "600": 600 * time.Second, "": 0} {
		t.Setenv("COLDPROBE_NO_WORKER_WARN_S", v)
		c, err := loadConfig()
		if err != nil || c.noWorkerWarn != want {
			t.Errorf("COLDPROBE_NO_WORKER_WARN_S=%q: %v, %v; want %v", v, c.noWorkerWarn, err, want)
		}
	}
	for _, bad := range []string{"0", "-5", "601", "ninety"} {
		t.Setenv("COLDPROBE_NO_WORKER_WARN_S", bad)
		_, err := loadConfig()
		// The exact text, so the value can't hide in it ("0" matches inside "600").
		const want = "COLDPROBE_NO_WORKER_WARN_S must be a whole number of seconds from 1 to 600 (the first-boot timeout)"
		if err == nil || err.Error() != want {
			t.Errorf("COLDPROBE_NO_WORKER_WARN_S=%q: err %v, want one naming the variable, not the value", bad, err)
		}
	}
}

// Bad values exit naming the variable but not echoing the value.
func TestLoadConfig_MaxWorkers(t *testing.T) {
	t.Setenv("COLDPROBE_FAKE", "1")
	for v, want := range map[string]int{"1": 1, "2": 2, "60": 60, "": 60} {
		t.Setenv("COLDPROBE_MAX_WORKERS", v)
		c, err := loadConfig()
		if err != nil || c.maxWorkers != want {
			t.Errorf("COLDPROBE_MAX_WORKERS=%q: %v, %v; want %v", v, c.maxWorkers, err, want)
		}
	}
	for _, bad := range []string{"0", "-1", "61", "2.5", "three"} {
		t.Setenv("COLDPROBE_MAX_WORKERS", bad)
		_, err := loadConfig()
		const want = "COLDPROBE_MAX_WORKERS must be a whole number from 1 to 60"
		if err == nil || err.Error() != want {
			t.Errorf("COLDPROBE_MAX_WORKERS=%q: err %v, want %q", bad, err, want)
		}
	}
}
