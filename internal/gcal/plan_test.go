package gcal

import (
	"testing"
	"time"
)

var now = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

func at(h float64) time.Time { return now.Add(time.Duration(h * float64(time.Hour))) }

func desired(peakH float64, title string) Desired {
	return Desired{Start: at(peakH - 0.5), End: at(peakH + 0.5), PeakTime: at(peakH), Title: title, Description: "d"}
}

func existing(id string, peakH float64, title string) Existing {
	return Existing{ID: id, Start: at(peakH - 0.5), End: at(peakH + 0.5), PeakTime: at(peakH), Title: title, Description: "d"}
}

func TestPlanUnchanged(t *testing.T) {
	p := MakePlan([]Desired{desired(10, "a")}, []Existing{existing("1", 10, "a")}, now)
	if p.Unchanged != 1 || len(p.Inserts)+len(p.Updates)+len(p.Deletes) != 0 {
		t.Fatalf("%+v", p)
	}
}

func TestPlanDriftWithinWindowUpdates(t *testing.T) {
	p := MakePlan([]Desired{desired(10.4, "b")}, []Existing{existing("1", 10, "a")}, now)
	if len(p.Updates) != 1 || p.Updates[0].Existing.ID != "1" || len(p.Inserts) != 0 || len(p.Deletes) != 0 {
		t.Fatalf("%+v", p)
	}
}

func TestPlanBeyondWindowIsInsertPlusDelete(t *testing.T) {
	p := MakePlan([]Desired{desired(14, "a")}, []Existing{existing("1", 10, "a")}, now)
	if len(p.Inserts) != 1 || len(p.Deletes) != 1 {
		t.Fatalf("%+v", p)
	}
}

func TestPlanDisappearedWindowDeleted(t *testing.T) {
	p := MakePlan(nil, []Existing{existing("1", 10, "a")}, now)
	if len(p.Deletes) != 1 {
		t.Fatalf("%+v", p)
	}
}

func TestPlanNewWindowInserted(t *testing.T) {
	p := MakePlan([]Desired{desired(10, "a"), desired(22.5, "b")}, []Existing{existing("1", 10, "a")}, now)
	if len(p.Inserts) != 1 || p.Inserts[0].Title != "b" || p.Unchanged != 1 {
		t.Fatalf("%+v", p)
	}
}

func TestPlanFrozenEventsUntouched(t *testing.T) {
	// started (ongoing) and changed: frozen, not updated
	ongoing := existing("1", 0.25, "old")
	// started and no longer forecast: frozen, not deleted
	gone := existing("2", -5, "old")
	p := MakePlan([]Desired{desired(0.3, "new")}, []Existing{ongoing, gone}, now)
	if p.Frozen != 2 || len(p.Updates) != 0 || len(p.Deletes) != 0 || len(p.Inserts) != 0 {
		t.Fatalf("%+v", p)
	}
}

func TestPlanOngoingWithoutEventCreatedOnce(t *testing.T) {
	p := MakePlan([]Desired{desired(0.25, "a")}, nil, now)
	if len(p.Inserts) != 1 {
		t.Fatalf("%+v", p)
	}
	// next run: that event now exists and has started -> frozen, no duplicate
	ins := p.Inserts[0]
	ex := Existing{ID: "x", Start: ins.Start, End: ins.End, PeakTime: ins.PeakTime, Title: "a", Description: "d"}
	p = MakePlan([]Desired{desired(0.3, "changed")}, []Existing{ex}, now.Add(time.Minute))
	if len(p.Inserts) != 0 || p.Frozen != 1 {
		t.Fatalf("%+v", p)
	}
}

func TestPlanEndedDesiredNotInserted(t *testing.T) {
	p := MakePlan([]Desired{desired(-3, "a")}, nil, now)
	if len(p.Inserts) != 0 || p.SkippedEnded != 1 {
		t.Fatalf("%+v", p)
	}
}

func TestPlanDuplicatesCleanedUp(t *testing.T) {
	p := MakePlan([]Desired{desired(10, "a")}, []Existing{existing("1", 10, "a"), existing("2", 10.1, "a")}, now)
	if p.Unchanged != 1 || len(p.Deletes) != 1 || p.Deletes[0].ID != "2" {
		t.Fatalf("%+v", p)
	}
}

func TestPlanNearestPeakWins(t *testing.T) {
	// two desired near one existing: closest one matches, other inserted
	p := MakePlan([]Desired{desired(8, "far"), desired(10.2, "near")}, []Existing{existing("1", 10, "x")}, now)
	if len(p.Updates) != 1 || p.Updates[0].Desired.Title != "near" || len(p.Inserts) != 1 || p.Inserts[0].Title != "far" {
		t.Fatalf("%+v", p)
	}
}
