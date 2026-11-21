// Command floodcal predicts high-water flood windows from NOAA data and
// prints them or syncs them into a Google Calendar.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/ragaskar/floodcal/internal/config"
	"github.com/ragaskar/floodcal/internal/gcal"
	"github.com/ragaskar/floodcal/internal/noaa"
	"github.com/ragaskar/floodcal/internal/output"
	"github.com/ragaskar/floodcal/internal/tide"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, config.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "floodcal: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	now := time.Now()
	res, err := config.Load(args, os.LookupEnv, stderr, now)
	if err != nil {
		return err
	}
	if res.ShowVersion {
		fmt.Fprintln(stdout, version)
		return nil
	}
	cfg := res.Config
	if cfg.ConfigPath != "" {
		fmt.Fprintf(stderr, "[config] loaded %s\n", cfg.ConfigPath)
	}

	ctx := context.Background()
	series, info, err := fetchSeries(ctx, cfg, stderr)
	if err != nil {
		return err
	}
	events := tide.BuildEvents(series, cfg.MinHeight, cfg.MergeGap, cfg.RoundMinutes, cfg.Location)
	meta := output.Meta{
		Station:       cfg.Station,
		Datum:         cfg.Datum,
		Timezone:      cfg.Timezone,
		MinHeight:     cfg.MinHeight,
		LocationLabel: cfg.LocationLabel,
		TitleTemplate: cfg.TitleTemplate,
		Source:        tide.Source(cfg.Source),
		Info:          info,
	}

	switch cfg.Mode {
	case "sync":
		return syncCalendar(ctx, cfg, series, events, meta, now, stdout, stderr)
	case "ics":
		path := cfg.Output
		if path == "" {
			path = fmt.Sprintf("tam-valley-flooding-%s-to-%s.ics",
				cfg.StartDate.Format("2006-01-02"), cfg.EndDate().Format("2006-01-02"))
		}
		if err := writeFile(path, func(w io.Writer) error { return output.WriteICS(w, events, meta, now) }); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Wrote %d events to %s\n", len(events), path)
		return nil
	case "json":
		return emit(cfg.Output, stdout, stderr, len(events), func(w io.Writer) error { return output.WriteJSON(w, events, meta) })
	default:
		return emit(cfg.Output, stdout, stderr, len(events), func(w io.Writer) error { return output.WriteList(w, events, meta) })
	}
}

// fetchSeries fetches NOAA data for the configured window and combines it
// according to --source.
func fetchSeries(ctx context.Context, cfg *config.Config, stderr io.Writer) ([]tide.Point, tide.SeriesInfo, error) {
	client := noaa.NewClient(stderr)
	begin := cfg.StartDate
	end := cfg.EndDate().Add(-6 * time.Minute) // NOAA end_date is inclusive
	source := tide.Source(cfg.Source)

	var pred, ofs []tide.Point
	var err error
	if source != tide.SourceForecast {
		pred, err = client.Fetch(ctx, noaa.ProductPredictions, cfg.Station, cfg.Datum, begin, end)
		if err != nil {
			return nil, tide.SeriesInfo{}, err
		}
	}
	if source != tide.SourceTide {
		ofs, err = client.Fetch(ctx, noaa.ProductOFS, cfg.Station, cfg.Datum, begin, end)
		if err != nil {
			if source == tide.SourceForecast {
				return nil, tide.SeriesInfo{}, err
			}
			fmt.Fprintf(stderr, "[warn] OFS forecast unavailable, falling back to tide table for all events (titles marked %q): %v\n",
				tide.EstimatedPrefix, err)
			ofs = nil
		} else if len(ofs) == 0 && source == tide.SourceHybrid {
			fmt.Fprintf(stderr, "[warn] OFS forecast returned no data, falling back to tide table for all events (titles marked %q)\n",
				tide.EstimatedPrefix)
		}
	}

	series, info, err := tide.BuildSeries(pred, ofs, source, cfg.AnomalyOffset)
	if err != nil {
		return nil, info, err
	}
	if len(series) == 0 {
		return nil, info, fmt.Errorf("NOAA returned no data for %s - %s", begin.Format("2006-01-02"), end.Format("2006-01-02 15:04"))
	}
	if len(ofs) > 0 {
		last := ofs[len(ofs)-1].T
		for _, p := range ofs {
			if p.T.After(last) {
				last = p.T
			}
		}
		fmt.Fprintf(stderr, "[data] OFS forecast through %s", last.In(cfg.Location).Format("2006-01-02 3:04 PM"))
		if info.OffsetApplied {
			fmt.Fprintf(stderr, "; anomaly offset %+.2f ft (from %d samples) applied after that", info.Offset, info.OverlapSamples)
		}
		fmt.Fprintln(stderr)
	}
	return series, info, nil
}

func syncCalendar(ctx context.Context, cfg *config.Config, series []tide.Point, events []tide.Event,
	meta output.Meta, now time.Time, stdout, stderr io.Writer) error {

	desired := make([]gcal.Desired, 0, len(events))
	for _, ev := range events {
		desired = append(desired, gcal.Desired{
			Start: ev.Start, End: ev.End, PeakTime: ev.PeakTime,
			Title: meta.Title(ev), Description: meta.Description(ev),
		})
	}
	from, to := series[0].T, series[len(series)-1].T

	api, err := gcal.NewClient(ctx, cfg.ServiceAccountKeyfile, cfg.CalendarID, cfg.Timezone, stderr)
	if err != nil {
		return err
	}
	res, err := gcal.Sync(ctx, api, cfg.Station, desired, from, to, now, cfg.DryRun, cfg.Location, stdout)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, res.Summary(cfg.DryRun))
	return nil
}

func emit(path string, stdout, stderr io.Writer, n int, write func(io.Writer) error) error {
	if path == "" {
		return write(stdout)
	}
	if err := writeFile(path, write); err != nil {
		return err
	}
	fmt.Fprintf(stderr, "Wrote %d events to %s\n", n, path)
	return nil
}

func writeFile(path string, write func(io.Writer) error) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := write(f); err != nil {
		f.Close()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return f.Close()
}
