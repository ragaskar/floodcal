// Package gcal syncs flood events into a Google Calendar.
package gcal

import (
	"sort"
	"time"
)

// MatchWindow is how far apart two peaks may be and still be considered the
// same flood event across runs. High tides are ~12.4 h apart.
const MatchWindow = 3 * time.Hour

// Desired is an event floodcal wants on the calendar.
type Desired struct {
	Start, End, PeakTime time.Time
	Title, Description   string
}

// Existing is a floodcal-created event already on the calendar.
type Existing struct {
	ID                   string
	Start, End, PeakTime time.Time
	Title, Description   string
}

// Update patches an existing event to match a desired one.
type Update struct {
	Existing Existing
	Desired  Desired
}

// Plan is the set of changes a sync will make.
type Plan struct {
	Inserts   []Desired
	Updates   []Update
	Deletes   []Existing
	Unchanged int
	// Frozen counts existing events left alone because they already started.
	Frozen int
	// SkippedEnded counts desired events not inserted because they already ended.
	SkippedEnded int
}

// frozen reports whether an existing event has started and so must never be
// modified or deleted.
func frozen(e Existing, now time.Time) bool { return !e.Start.After(now) }

// MakePlan matches desired events to existing ones by nearest peak (within
// MatchWindow) and returns the changes needed. existing must already be
// limited to events whose peak lies within the range the fetched data covers.
func MakePlan(desired []Desired, existing []Existing, now time.Time) Plan {
	type pair struct {
		d, e int
		dist time.Duration
	}
	var pairs []pair
	for di, d := range desired {
		for ei, e := range existing {
			dist := d.PeakTime.Sub(e.PeakTime)
			if dist < 0 {
				dist = -dist
			}
			if dist <= MatchWindow {
				pairs = append(pairs, pair{di, ei, dist})
			}
		}
	}
	sort.SliceStable(pairs, func(i, j int) bool { return pairs[i].dist < pairs[j].dist })

	dMatch := make([]int, len(desired))
	eMatched := make([]bool, len(existing))
	for i := range dMatch {
		dMatch[i] = -1
	}
	for _, p := range pairs {
		if dMatch[p.d] != -1 || eMatched[p.e] {
			continue
		}
		dMatch[p.d] = p.e
		eMatched[p.e] = true
	}

	var plan Plan
	for di, d := range desired {
		ei := dMatch[di]
		if ei == -1 {
			if !d.End.After(now) {
				plan.SkippedEnded++
				continue
			}
			plan.Inserts = append(plan.Inserts, d)
			continue
		}
		e := existing[ei]
		switch {
		case frozen(e, now):
			plan.Frozen++
		case same(d, e):
			plan.Unchanged++
		default:
			plan.Updates = append(plan.Updates, Update{Existing: e, Desired: d})
		}
	}
	for ei, e := range existing {
		if eMatched[ei] {
			continue
		}
		if frozen(e, now) {
			plan.Frozen++
			continue
		}
		plan.Deletes = append(plan.Deletes, e)
	}
	return plan
}

func same(d Desired, e Existing) bool {
	return d.Start.Equal(e.Start) && d.End.Equal(e.End) && d.PeakTime.Equal(e.PeakTime) &&
		d.Title == e.Title && d.Description == e.Description
}
