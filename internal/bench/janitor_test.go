package bench

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/josh10111/coldprobe/internal/fakerunpod"
	"github.com/josh10111/coldprobe/internal/runpod"
	"github.com/josh10111/coldprobe/internal/store"
)

const testKey = "fake-runpod-test-key" // gitleaks:allow (test fixture)

// newFake starts a fake Runpod with one of our endpoints and one foreign one.
func newFake(t *testing.T) (fake *fakerunpod.Fake, ours, other string) {
	t.Helper()
	fake = fakerunpod.New(fakerunpod.TestOptions())
	t.Cleanup(fake.Close)
	ours = fake.AddEndpoint(runpod.NamePrefix + "a")
	other = fake.AddEndpoint("other-b")
	return fake, ours, other
}

func newMgmt(t *testing.T, baseURL string) *runpod.Mgmt {
	t.Helper()
	m, err := runpod.NewMgmt(baseURL, testKey)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestJanitor_DryRunListsOnlyPrefix(t *testing.T) {
	fake, ours, _ := newFake(t)

	got, err := Janitor(context.Background(), newMgmt(t, fake.MgmtURL()), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != ours || got[0].Name != runpod.NamePrefix+"a" {
		t.Errorf("Janitor = %+v, want only %s", got, ours)
	}
	if live := fake.LiveEndpoints(); len(live) != 2 {
		t.Errorf("dry run deleted something: live = %v", live)
	}
}

func TestJanitor_DeleteRemovesOnlyPrefix(t *testing.T) {
	fake, ours, other := newFake(t)

	got, err := Janitor(context.Background(), newMgmt(t, fake.MgmtURL()), true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != ours {
		t.Errorf("Janitor = %+v, want only %s", got, ours)
	}
	if live := fake.LiveEndpoints(); !slices.Equal(live, []string{other}) {
		t.Errorf("live = %v, want only %s", live, other)
	}
}

// The fake lists endpoints in ID order, so the stuck one is tried first.
func TestJanitor_DeleteFailureContinues(t *testing.T) {
	fake := fakerunpod.New(fakerunpod.TestOptions())
	t.Cleanup(fake.Close)
	stuck := fake.AddEndpoint(runpod.NamePrefix + "stuck")
	fine := fake.AddEndpoint(runpod.NamePrefix + "fine")

	m := newMgmt(t, stuckProxy(t, fake, stuck))

	got, err := Janitor(context.Background(), m, true, nil)
	if err == nil || !strings.Contains(err.Error(), stuck) {
		t.Errorf("err = %v, want one naming %s", err, stuck)
	}
	if len(got) != 2 {
		t.Fatalf("Janitor returned %d endpoints, want both matches", len(got))
	}
	for _, e := range got {
		if failed := e.Err != nil; failed != (e.ID == stuck) {
			t.Errorf("%s: Err = %v, want an error only for %s", e.ID, e.Err, stuck)
		}
	}
	if live := fake.LiveEndpoints(); !slices.Equal(live, []string{stuck}) {
		t.Errorf("live = %v, want only %s left (%s deleted)", live, stuck, fine)
	}
}

// stuckProxy fronts the fake's management API and answers 403 to every
// DELETE of stuck: 5xx would be retried and 404 counts as success.
func stuckProxy(t *testing.T, fake *fakerunpod.Fake, stuck string) string {
	target, _ := url.Parse(fake.MgmtURL())
	proxy := httputil.NewSingleHostReverseProxy(target)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/"+stuck) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestSweep_LogsDeletedVsFailed(t *testing.T) {
	fake := fakerunpod.New(fakerunpod.TestOptions())
	t.Cleanup(fake.Close)
	stuck := fake.AddEndpoint(runpod.NamePrefix + "stuck")
	fine := fake.AddEndpoint(runpod.NamePrefix + "fine")
	st, err := store.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	var buf bytes.Buffer

	err = Sweep(context.Background(), newMgmt(t, stuckProxy(t, fake, stuck)), st, slog.New(slog.NewTextHandler(&buf, nil)))

	if err == nil {
		t.Error("Sweep = nil, want the failed delete")
	}
	lines := strings.Split(buf.String(), "\n")
	has := func(msg, id string) bool {
		for _, l := range lines {
			if strings.Contains(l, msg) && strings.Contains(l, "id="+id) {
				return true
			}
		}
		return false
	}
	if !has(`msg="janitor: deleted endpoint"`, fine) {
		t.Errorf("no 'deleted' line for %s in:\n%s", fine, buf.String())
	}
	if !has(`msg="janitor: delete failed"`, stuck) || has("deleted endpoint", stuck) {
		t.Errorf("want a 'delete failed' line (and no 'deleted' line) for %s in:\n%s", stuck, buf.String())
	}
}

func TestSweep_StampsDeletedAt(t *testing.T) {
	ctx := context.Background()
	fake := fakerunpod.New(fakerunpod.TestOptions())
	t.Cleanup(fake.Close)
	st, err := store.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Now()
	id := runIDAt(now, "aaaa")
	if err := st.CreateRun(ctx, store.Run{ID: id, CreatedAt: now, Status: "aborted_crash"}); err != nil {
		t.Fatal(err)
	}
	ep := fake.AddEndpoint(endpointName(id, 0))
	if _, err := st.CreateConfig(ctx, store.Config{RunID: id, Pool: "AMPERE_16", FlashBoot: "OFF", EndpointID: ep, Status: "failed", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}

	if err := Sweep(ctx, newMgmt(t, fake.MgmtURL()), st, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}

	rd, err := st.GetRun(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if c := rd.Configs[0]; c.DeletedAt == nil {
		t.Errorf("config with endpoint %s: DeletedAt nil after the janitor deleted it", ep)
	}
	if live := fake.LiveEndpoints(); len(live) != 0 {
		t.Errorf("live endpoints = %v, want none", live)
	}
}

func runIDAt(t time.Time, suffix string) string {
	return t.UTC().Format(runIDTime) + "-" + suffix
}

// Run IDs and their parse are both UTC, so the ages hold in any time zone.
func TestJanitor_SkipRule(t *testing.T) {
	ctx := context.Background()
	fake := fakerunpod.New(fakerunpod.TestOptions())
	t.Cleanup(fake.Close)
	st, err := store.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	now := time.Now()
	finished, live := runIDAt(now, "aaaa"), runIDAt(now, "bbbb")
	for id, status := range map[string]string{finished: "completed", live: "running"} {
		if err := st.CreateRun(ctx, store.Run{ID: id, CreatedAt: now, Status: status}); err != nil {
			t.Fatal(err)
		}
	}
	ep := func(runID string) string { return fake.AddEndpoint(endpointName(runID, 0)) }
	finishedEp := ep(finished)                                    // ours, finished: delete
	liveEp := ep(live)                                            // ours, still running: skip
	youngEp := ep(runIDAt(now.Add(-10*time.Minute), "cccc"))      // unknown, 10 min old: skip
	oldEp := ep(runIDAt(now.Add(-2*time.Hour), "dddd"))           // unknown, 2 h old: delete
	oddEp := fake.AddEndpoint(runpod.NamePrefix + "not-a-run-id") // unparseable: delete
	other := fake.AddEndpoint("other-b")                          // not ours: never touched

	skip, err := SkipRule(ctx, st, now)
	if err != nil {
		t.Fatal(err)
	}
	m := newMgmt(t, fake.MgmtURL())

	got, err := Janitor(ctx, m, false, skip)
	if err != nil {
		t.Fatal(err)
	}
	reasons := map[string]string{}
	for _, e := range got {
		reasons[e.ID] = e.Skip
	}
	if len(got) != 5 {
		t.Errorf("dry run listed %d endpoints, want the 5 prefixed ones: %+v", len(got), got)
	}
	for _, id := range []string{finishedEp, oldEp, oddEp} {
		if r, ok := reasons[id]; !ok || r != "" {
			t.Errorf("%s: listed %v, skip %q; want listed for deletion", id, ok, r)
		}
	}
	if r := reasons[liveEp]; !strings.Contains(r, "still running") {
		t.Errorf("live run's endpoint: skip %q, want one saying its run is still running", r)
	}
	if r := reasons[youngEp]; !strings.Contains(r, "another coldprobe instance") || !strings.Contains(r, "-force") {
		t.Errorf("young unknown endpoint: skip %q, want one naming another instance and -force", r)
	}
	if live := fake.LiveEndpoints(); len(live) != 6 {
		t.Errorf("dry run deleted something: live = %v", live)
	}

	if _, err := Janitor(ctx, m, true, skip); err != nil {
		t.Fatal(err)
	}
	want := []string{liveEp, youngEp, other}
	slices.Sort(want)
	if live := fake.LiveEndpoints(); !slices.Equal(live, want) {
		t.Errorf("live after delete = %v, want %v", live, want)
	}
}

func TestRunOf(t *testing.T) {
	id := newRunID()
	got, started, ok := runOf(endpointName(id, 3))
	if !ok || got != id || time.Since(started) > time.Minute || time.Since(started) < -time.Second {
		t.Errorf("runOf(endpointName(%q, 3)) = %q, %v, %v", id, got, started, ok)
	}
	// Trailing text after the ID must not make a young endpoint unparseable.
	for _, name := range []string{
		endpointName(id, 3) + " -fb",
		endpointName(id, 12) + "-x-y",
		runpod.NamePrefix + id + "-probe",
	} {
		if got, _, ok := runOf(name); !ok || got != id {
			t.Errorf("runOf(%q) = %q, %v; want %q", name, got, ok, id)
		}
	}
	for _, name := range []string{
		runpod.NamePrefix + "a",
		runpod.NamePrefix + "20261107-150405-a1b2",   // no index
		runpod.NamePrefix + "20261399-150405-a1b2-0", // month 13
		runpod.NamePrefix + "20261107-150405-a1bz-0", // not hex
		runpod.NamePrefix + "20261107-150405-a1b2x0", // no '-' after the ID
		runpod.NamePrefix + "day1-1762500000",        // the probe's old name
		"other-20261107-150405-a1b2-0",
	} {
		if _, _, ok := runOf(name); ok {
			t.Errorf("runOf(%q) ok, want not a run's endpoint", name)
		}
	}
}
