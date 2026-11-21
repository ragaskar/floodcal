//go:build live

// Live smoke tests against a real Google Calendar. They only build with
// `-tags live` (see `make smoke`), so `make test` never touches the network.
//
// Each test tags its events with a unique synthetic station ID
// ("smoketest-<nanos>"). floodcal only ever lists/modifies events for the
// station it is syncing, so real events on the calendar are never touched.
// Everything created is deleted on cleanup.
package gcal

import (
	"context"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/ragaskar/floodcal/internal/config"
)

const smokeConfigEnv = "FLOODCAL_SMOKE_CONFIG"

type liveEnv struct {
	client  *Client
	station string
	loc     *time.Location
	now     time.Time
	from    time.Time
	to      time.Time
}

func setupLive(t *testing.T) *liveEnv {
	t.Helper()
	path := os.Getenv(smokeConfigEnv)
	if path == "" {
		t.Fatalf("%s must point to a floodcal config with calendar-id and service-account-keyfile (use make smoke SMOKE_CONFIG=...)", smokeConfigEnv)
	}
	res, err := config.Load([]string{"--config", path, "--mode", "sync"}, os.LookupEnv, io.Discard, time.Now())
	if err != nil {
		t.Fatalf("loading %s: %v", path, err)
	}
	cfg := res.Config
	ctx := context.Background()
	c, err := NewClient(ctx, cfg.ServiceAccountKeyfile, cfg.CalendarID, cfg.Timezone, testLog{t})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Minute)
	env := &liveEnv{
		client:  c,
		station: fmt.Sprintf("smoketest-%d", time.Now().UnixNano()),
		loc:     cfg.Location,
		now:     now,
		from:    now.Add(-6 * time.Hour),
		to:      now.Add(4 * 24 * time.Hour),
	}
	t.Logf("calendar %s, station tag %s", cfg.CalendarID, env.station)
	t.Cleanup(func() { env.cleanup(t) })
	return env
}

// cleanup deletes every event carrying this test's station tag.
func (e *liveEnv) cleanup(t *testing.T) {
	ctx := context.Background()
	evs, err := e.client.List(ctx, e.station, e.from.Add(-24*time.Hour), e.to.Add(24*time.Hour))
	if err != nil {
		t.Errorf("cleanup: listing %s events: %v (delete them manually)", e.station, err)
		return
	}
	for _, ev := range evs {
		if err := e.client.Delete(ctx, ev.ID); err != nil {
			t.Errorf("cleanup: deleting %s (%s): %v", ev.ID, ev.Title, err)
		}
	}
	t.Logf("cleanup: deleted %d %s events", len(evs), e.station)
}

func (e *liveEnv) desired(start, end, peak time.Time, title string) Desired {
	return Desired{Start: start, End: end, PeakTime: peak, Title: "[floodcal smoke test] " + title,
		Description: "Created by floodcal live smoke tests; safe to delete. Station tag " + e.station}
}

func (e *liveEnv) insert(t *testing.T, d Desired) {
	t.Helper()
	if err := e.client.Insert(context.Background(), e.station, d); err != nil {
		t.Fatal(err)
	}
}

func (e *liveEnv) list(t *testing.T) []Existing {
	t.Helper()
	evs, err := e.client.List(context.Background(), e.station, e.from, e.to)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func (e *liveEnv) sync(t *testing.T, desired []Desired, now time.Time) Plan {
	t.Helper()
	res, err := Sync(context.Background(), e.client, e.station, desired, e.from, e.to, now, false, e.loc, testLog{t})
	if err != nil {
		t.Fatal(err)
	}
	t.Log(res.Summary(false))
	return res.Plan
}

func byTitle(evs []Existing) map[string]Existing {
	m := make(map[string]Existing, len(evs))
	for _, e := range evs {
		m[e.Title] = e
	}
	return m
}

// TestLiveFrozenEvents: events that have started are never patched or
// deleted, while not-yet-started stale events are deleted.
func TestLiveFrozenEvents(t *testing.T) {
	e := setupLive(t)
	n := e.now

	ongoing := e.desired(n.Add(-30*time.Minute), n.Add(30*time.Minute), n, "ongoing")
	pastGone := e.desired(n.Add(-3*time.Hour), n.Add(-2*time.Hour), n.Add(-150*time.Minute), "started, no longer forecast")
	futureGone := e.desired(n.Add(20*time.Hour), n.Add(21*time.Hour), n.Add(20*time.Hour+30*time.Minute), "future, no longer forecast")
	for _, d := range []Desired{ongoing, pastGone, futureGone} {
		e.insert(t, d)
	}

	// New forecast: the ongoing window changed; the other two disappeared.
	changed := ongoing
	changed.Title += " (CHANGED - must not be applied)"
	changed.End = n.Add(45 * time.Minute)
	plan := e.sync(t, []Desired{changed}, n)

	if plan.Frozen != 2 || len(plan.Deletes) != 1 || len(plan.Updates) != 0 || len(plan.Inserts) != 0 {
		t.Fatalf("plan: frozen %d deletes %d updates %d inserts %d; want 2/1/0/0",
			plan.Frozen, len(plan.Deletes), len(plan.Updates), len(plan.Inserts))
	}
	got := byTitle(e.list(t))
	if len(got) != 2 {
		t.Fatalf("want 2 events left, got %d: %v", len(got), got)
	}
	if g, ok := got[ongoing.Title]; !ok || !g.End.Equal(ongoing.End) {
		t.Errorf("ongoing event was modified: %+v", g)
	}
	if _, ok := got[pastGone.Title]; !ok {
		t.Errorf("started event was deleted")
	}
	if _, ok := got[futureGone.Title]; ok {
		t.Errorf("future stale event was not deleted")
	}
}

// TestLiveOngoingWindowCreatedOnce: a window already in progress with no
// event is created once, then frozen (no duplicate, no patch) on later runs.
func TestLiveOngoingWindowCreatedOnce(t *testing.T) {
	e := setupLive(t)
	n := e.now
	d := e.desired(n.Add(-20*time.Minute), n.Add(40*time.Minute), n.Add(10*time.Minute), "mid-flood first run")

	if plan := e.sync(t, []Desired{d}, n); len(plan.Inserts) != 1 {
		t.Fatalf("first run: want 1 insert, got %+v", plan)
	}
	again := d
	again.Title += " (CHANGED - must not be applied)"
	again.PeakTime = d.PeakTime.Add(10 * time.Minute)
	if plan := e.sync(t, []Desired{again}, n.Add(time.Minute)); plan.Frozen != 1 || len(plan.Inserts)+len(plan.Updates)+len(plan.Deletes) != 0 {
		t.Fatalf("second run: want frozen only, got %+v", plan)
	}
	got := e.list(t)
	if len(got) != 1 || got[0].Title != d.Title {
		t.Fatalf("want exactly the original event, got %+v", got)
	}
}

// TestLivePagination: List follows NextPageToken, and sync matches events
// spread across pages (no spurious inserts/deletes).
func TestLivePagination(t *testing.T) {
	e := setupLive(t)
	e.client.pageSize = 5
	const count = 12
	var want []Desired
	for i := 0; i < count; i++ {
		start := e.now.Add(time.Duration(2+6*i) * time.Hour) // 6 h apart: beyond the ±3 h match window
		d := e.desired(start, start.Add(time.Hour), start.Add(30*time.Minute), fmt.Sprintf("page test %02d", i))
		e.insert(t, d)
		want = append(want, d)
	}

	got := e.list(t)
	if len(got) != count {
		t.Fatalf("List with page size %d returned %d events, want %d", e.client.pageSize, len(got), count)
	}
	titles := byTitle(got)
	for _, d := range want {
		if _, ok := titles[d.Title]; !ok {
			t.Errorf("missing %q", d.Title)
		}
	}

	plan := e.sync(t, want, e.now)
	if plan.Unchanged != count || len(plan.Inserts)+len(plan.Updates)+len(plan.Deletes) != 0 {
		t.Fatalf("sync across pages: %+v", plan)
	}
}

type testLog struct{ t *testing.T }

func (l testLog) Write(p []byte) (int, error) {
	l.t.Log(string(p))
	return len(p), nil
}
