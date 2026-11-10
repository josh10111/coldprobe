package bench

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/josh10111/coldprobe/internal/runpod"
	"github.com/josh10111/coldprobe/internal/store"
)

// janitorGrace is how long an endpoint whose run isn't in our database is
// left alone, since another coldprobe instance on the account may own it.
// A default run ends well within it, but a very long one can outlive it.
const janitorGrace = time.Hour

// Leftover is one endpoint with our name prefix that the janitor found.
type Leftover struct {
	runpod.Endpoint
	Skip string // why it was left alone; "" means deleted (or would be)
	Err  error  // why its delete failed
}

// Janitor lists endpoints named with runpod.NamePrefix and, if del is true,
// deletes those skip doesn't spare (nil skip deletes all). The prefix is the
// only ownership mark the API offers. A failed delete doesn't stop the rest;
// failures come back joined.
func Janitor(ctx context.Context, m *runpod.Mgmt, del bool, skip func(runpod.Endpoint) string) ([]Leftover, error) {
	all, err := m.ListEndpoints(ctx)
	if err != nil {
		return nil, fmt.Errorf("janitor: %w", err)
	}

	var ours []Leftover
	for _, e := range all {
		if !strings.HasPrefix(e.Name, runpod.NamePrefix) {
			continue
		}
		l := Leftover{Endpoint: e}
		if skip != nil {
			l.Skip = skip(e)
		}
		ours = append(ours, l)
	}
	if !del {
		return ours, nil
	}

	var errs []error
	for i, e := range ours {
		if e.Skip != "" {
			continue
		}
		if err := m.DeleteEndpoint(ctx, e.ID); err != nil {
			ours[i].Err = fmt.Errorf("janitor: %w", err)
			errs = append(errs, ours[i].Err)
		}
	}
	return ours, errors.Join(errs...)
}

// SkipRule returns the automatic janitor's rule, judged against the runs in st:
// skip a live run's endpoints and an unknown run's younger than janitorGrace;
// delete the rest, including names runOf can't parse.
func SkipRule(ctx context.Context, st *store.Store, now time.Time) (func(runpod.Endpoint) string, error) {
	runs, err := st.ListRuns(ctx)
	if err != nil {
		return nil, fmt.Errorf("janitor: %w", err)
	}
	status := make(map[string]string, len(runs))
	for _, r := range runs {
		status[r.ID] = r.Status
	}
	return func(e runpod.Endpoint) string {
		id, started, ok := runOf(e.Name)
		if !ok {
			return ""
		}
		s, known := status[id]
		switch age := now.Sub(started); {
		case known && s == "running":
			return "its run " + id + " is still running"
		case known:
			return ""
		case age < janitorGrace:
			return fmt.Sprintf("run %s is not in this database and started %v ago: it may belong to another coldprobe instance; "+
				"`coldprobe janitor -delete -force` removes it", id, age.Round(time.Second))
		}
		return ""
	}, nil
}

// runOf parses a name made by endpointName back into its run ID and UTC start
// time. The ID is read from its fixed position after the prefix, so a suffix
// someone appended can't make a young endpoint unparseable (and deleted).
func runOf(name string) (runID string, started time.Time, ok bool) {
	rest, found := strings.CutPrefix(name, runpod.NamePrefix)
	if !found || len(rest) <= runIDLen || rest[runIDLen] != '-' {
		return "", time.Time{}, false
	}
	runID = rest[:runIDLen]
	ts, suffix := runID[:len(runIDTime)], runID[len(runIDTime):]
	started, err := time.Parse(runIDTime, ts)
	if err != nil || suffix[0] != '-' {
		return "", time.Time{}, false
	}
	if _, err := hex.DecodeString(suffix[1:]); err != nil {
		return "", time.Time{}, false
	}
	return runID, started, true
}

// Sweep is the automatic janitor: SkipRule then Janitor with deletes, logging
// what it did and stamping deletedAt on configs that owned a deleted endpoint.
// Call it only when no run of ours is active.
func Sweep(ctx context.Context, m *runpod.Mgmt, st *store.Store, log *slog.Logger) error {
	skip, err := SkipRule(ctx, st, time.Now())
	if err != nil {
		return err
	}
	found, err := Janitor(ctx, m, true, skip)
	deleted, skipped := 0, 0
	for _, e := range found {
		switch {
		case e.Skip != "":
			skipped++
			log.Warn("janitor: skipped endpoint", "id", e.ID, "name", e.Name, "reason", e.Skip)
		case e.Err != nil:
			log.Error("janitor: delete failed", "id", e.ID, "name", e.Name, "err", e.Err)
		default:
			deleted++
			log.Warn("janitor: deleted endpoint", "id", e.ID, "name", e.Name)
			// WithoutCancel: record finished deletes even if the sweep's deadline ran out.
			if err := st.MarkEndpointDeleted(context.WithoutCancel(ctx), e.ID, time.Now()); err != nil {
				log.Warn("janitor: endpoint deleted but not marked in the database", "id", e.ID, "err", err)
			}
		}
	}
	if err != nil {
		return err
	}
	log.Info("janitor done", "deleted", deleted, "skipped", skipped)
	return nil
}
