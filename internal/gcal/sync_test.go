package gcal

import (
	"context"
	"io"
	"testing"
	"time"
)

type fakeAPI struct {
	events  map[string]Existing
	nextID  int
	inserts int
	patches int
	deletes int
}

func newFake() *fakeAPI { return &fakeAPI{events: map[string]Existing{}} }

func (f *fakeAPI) List(_ context.Context, _ string, from, to time.Time) ([]Existing, error) {
	var out []Existing
	for _, e := range f.events {
		if e.End.After(from) && e.Start.Before(to) {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeAPI) Insert(_ context.Context, _ string, d Desired) error {
	f.nextID++
	id := string(rune('a' + f.nextID))
	f.events[id] = Existing{ID: id, Start: d.Start, End: d.End, PeakTime: d.PeakTime, Title: d.Title, Description: d.Description}
	f.inserts++
	return nil
}

func (f *fakeAPI) Patch(_ context.Context, _ string, id string, d Desired) error {
	f.events[id] = Existing{ID: id, Start: d.Start, End: d.End, PeakTime: d.PeakTime, Title: d.Title, Description: d.Description}
	f.patches++
	return nil
}

func (f *fakeAPI) Delete(_ context.Context, id string) error {
	delete(f.events, id)
	f.deletes++
	return nil
}

func TestSyncIdempotent(t *testing.T) {
	api := newFake()
	want := []Desired{desired(10, "a"), desired(22, "b")}
	from, to := at(0), at(48)
	if _, err := Sync(context.Background(), api, "s", want, from, to, now, false, time.UTC, io.Discard); err != nil {
		t.Fatal(err)
	}
	r, err := Sync(context.Background(), api, "s", want, from, to, now, false, time.UTC, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if api.inserts != 2 || r.Plan.Unchanged != 2 || len(r.Plan.Inserts)+len(r.Plan.Updates)+len(r.Plan.Deletes) != 0 {
		t.Fatalf("api %+v plan %+v", api, r.Plan)
	}
}

func TestSyncDryRunMakesNoWrites(t *testing.T) {
	api := newFake()
	r, err := Sync(context.Background(), api, "s", []Desired{desired(10, "a")}, at(0), at(48), now, true, time.UTC, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if api.inserts != 0 || len(r.Plan.Inserts) != 1 {
		t.Fatalf("api %+v plan %+v", api, r.Plan)
	}
}

func TestSyncNeverDeletesOutsideCoveredRange(t *testing.T) {
	api := newFake()
	api.events["far"] = existing("far", 100, "beyond data")
	// Covered range ends at 48h; List overlap could include an event straddling
	// the edge, but its peak is outside so it must survive.
	api.events["edge"] = Existing{ID: "edge", Start: at(47.5), End: at(49), PeakTime: at(48.5), Title: "edge"}
	if _, err := Sync(context.Background(), api, "s", nil, at(0), at(48), now, false, time.UTC, io.Discard); err != nil {
		t.Fatal(err)
	}
	if api.deletes != 0 {
		t.Fatalf("deleted %d events outside the covered range", api.deletes)
	}
}
