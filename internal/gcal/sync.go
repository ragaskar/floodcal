package gcal

import (
	"context"
	"fmt"
	"io"
	"time"
)

// Result summarizes a sync run.
type Result struct {
	Plan Plan
}

func (r Result) Summary(dryRun bool) string {
	prefix := ""
	if dryRun {
		prefix = "[dry-run] would have "
	}
	p := r.Plan
	return fmt.Sprintf("%screated %d, updated %d, deleted %d, unchanged %d, frozen %d",
		prefix, len(p.Inserts), len(p.Updates), len(p.Deletes), p.Unchanged, p.Frozen)
}

// Sync reconciles the calendar with desired for the range [from, to], which
// must be the span actually covered by fetched data. Existing events whose
// peak lies outside that span are never touched.
func Sync(ctx context.Context, api API, station string, desired []Desired, from, to, now time.Time,
	dryRun bool, loc *time.Location, log io.Writer) (Result, error) {

	all, err := api.List(ctx, station, from, to)
	if err != nil {
		return Result{}, err
	}
	var existing []Existing
	for _, e := range all {
		if e.PeakTime.Before(from) || e.PeakTime.After(to) {
			continue
		}
		existing = append(existing, e)
	}

	plan := MakePlan(desired, existing, now)
	res := Result{Plan: plan}

	verb := func(s string) string {
		if dryRun {
			return "[dry-run] " + s
		}
		return s
	}
	when := func(t time.Time) string { return t.In(loc).Format("2006-01-02 3:04 PM") }

	for _, d := range plan.Inserts {
		fmt.Fprintf(log, "%s %s  %s\n", verb("create"), when(d.Start), d.Title)
		if !dryRun {
			if err := api.Insert(ctx, station, d); err != nil {
				return res, err
			}
		}
	}
	for _, u := range plan.Updates {
		fmt.Fprintf(log, "%s %s  %s -> %s\n", verb("update"), when(u.Desired.Start), u.Existing.Title, u.Desired.Title)
		if !dryRun {
			if err := api.Patch(ctx, station, u.Existing.ID, u.Desired); err != nil {
				return res, err
			}
		}
	}
	for _, e := range plan.Deletes {
		fmt.Fprintf(log, "%s %s  %s\n", verb("delete"), when(e.Start), e.Title)
		if !dryRun {
			if err := api.Delete(ctx, e.ID); err != nil {
				return res, err
			}
		}
	}
	return res, nil
}
