// Package tide turns a water-level time series into above-threshold flood
// windows. The window logic is a direct port of generate_flood_calendar.py.
package tide

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Point is one water-level sample.
type Point struct {
	T time.Time
	V float64
	// Forecast is true when the value came from the OFS forecast (weather
	// included); false when it came from the astronomical tide table.
	Forecast bool
}

// Event is a merged, rounded flood window.
type Event struct {
	Start      time.Time
	End        time.Time
	PeakTime   time.Time
	PeakHeight float64
	// Estimated is true when any sample in the window lacked OFS forecast data.
	Estimated bool
}

type rawWindow struct {
	start, end time.Time
	samples    []Point
}

// interpolateCrossing linearly interpolates the instant at which the line
// through (t0,v0)-(t1,v1) reaches threshold.
func interpolateCrossing(t0 time.Time, v0 float64, t1 time.Time, v1 float64, threshold float64) time.Time {
	if v1 == v0 {
		return t0
	}
	frac := (threshold - v0) / (v1 - v0)
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	return t0.Add(time.Duration(float64(t1.Sub(t0)) * frac))
}

func findRawWindows(points []Point, threshold float64) []rawWindow {
	var windows []rawWindow
	var cur *rawWindow
	for i, p := range points {
		above := p.V >= threshold
		switch {
		case above && cur == nil:
			start := p.T
			if i > 0 {
				prev := points[i-1]
				start = interpolateCrossing(prev.T, prev.V, p.T, p.V, threshold)
			}
			cur = &rawWindow{start: start, samples: []Point{p}}
		case above && cur != nil:
			cur.samples = append(cur.samples, p)
		case !above && cur != nil:
			prev := points[i-1]
			cur.end = interpolateCrossing(prev.T, prev.V, p.T, p.V, threshold)
			windows = append(windows, *cur)
			cur = nil
		}
	}
	if cur != nil {
		cur.end = points[len(points)-1].T
		windows = append(windows, *cur)
	}
	return windows
}

func mergeWindows(windows []rawWindow, mergeGap time.Duration) []rawWindow {
	if len(windows) == 0 {
		return nil
	}
	merged := []rawWindow{copyWindow(windows[0])}
	for _, w := range windows[1:] {
		last := &merged[len(merged)-1]
		if w.start.Sub(last.end) <= mergeGap {
			last.end = w.end
			last.samples = append(last.samples, w.samples...)
		} else {
			merged = append(merged, copyWindow(w))
		}
	}
	return merged
}

func copyWindow(w rawWindow) rawWindow {
	w.samples = append([]Point(nil), w.samples...)
	return w
}

// roundToNearest rounds t to the nearest step on the local wall clock of loc
// (ties round up), matching the Python implementation.
func roundToNearest(t time.Time, step time.Duration, loc *time.Location) time.Time {
	lt := t.In(loc)
	_, offset := lt.Zone()
	wall := lt.UnixNano() + int64(offset)*int64(time.Second)
	s := int64(step)
	rounded := floorDiv(wall+s/2, s) * s
	return time.Unix(0, rounded-int64(offset)*int64(time.Second)).In(loc)
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

// BuildEvents detects, merges and rounds flood windows. points must be sorted
// by time.
func BuildEvents(points []Point, minHeight float64, mergeGapMinutes, roundMinutes int, loc *time.Location) []Event {
	raw := findRawWindows(points, minHeight)
	merged := mergeWindows(raw, time.Duration(mergeGapMinutes)*time.Minute)
	step := time.Duration(roundMinutes) * time.Minute

	events := make([]Event, 0, len(merged))
	for _, w := range merged {
		start := roundToNearest(w.start, step, loc)
		end := roundToNearest(w.end, step, loc)
		if !end.After(start) {
			end = start.Add(step)
		}
		peak := w.samples[0]
		estimated := false
		for _, s := range w.samples {
			if s.V > peak.V { // strict: first sample wins ties, like Python's max()
				peak = s
			}
			if !s.Forecast {
				estimated = true
			}
		}
		events = append(events, Event{
			Start:      start,
			End:        end,
			PeakTime:   peak.T.In(loc),
			PeakHeight: peak.V,
			Estimated:  estimated,
		})
	}
	return events
}

// Source selects which NOAA data feeds the series.
type Source string

const (
	SourceHybrid   Source = "hybrid"
	SourceForecast Source = "forecast"
	SourceTide     Source = "tide"
)

// SeriesInfo describes how a series was assembled.
type SeriesInfo struct {
	// Offset is the anomaly (mean OFS minus tide table) added to tide-table
	// samples. Zero when not applied.
	Offset float64
	// OffsetApplied reports whether Offset was added to tide-table samples.
	OffsetApplied bool
	// OverlapSamples is how many timestamps had both OFS and tide data.
	OverlapSamples int
	// ForecastSamples is how many samples in the series came from OFS.
	ForecastSamples int
}

// BuildSeries combines tide-table predictions and OFS forecast samples
// according to source. Inputs need not be sorted; output is sorted.
func BuildSeries(pred, ofs []Point, source Source, anomalyOffset bool) ([]Point, SeriesInfo, error) {
	var info SeriesInfo
	switch source {
	case SourceTide:
		out := markAll(pred, false)
		return out, info, nil
	case SourceForecast:
		if len(ofs) == 0 {
			return nil, info, fmt.Errorf("no OFS forecast data available")
		}
		out := markAll(ofs, true)
		info.ForecastSamples = len(out)
		return out, info, nil
	case SourceHybrid:
	default:
		return nil, info, fmt.Errorf("unknown source %q", source)
	}

	predBy := make(map[int64]float64, len(pred))
	for _, p := range pred {
		predBy[p.T.Unix()] = p.V
	}
	ofsBy := make(map[int64]float64, len(ofs))
	var sum float64
	for _, f := range ofs {
		ofsBy[f.T.Unix()] = f.V
		if pv, ok := predBy[f.T.Unix()]; ok {
			sum += f.V - pv
			info.OverlapSamples++
		}
	}
	if anomalyOffset && info.OverlapSamples > 0 {
		info.Offset = sum / float64(info.OverlapSamples)
		info.OffsetApplied = true
	}

	out := make([]Point, 0, len(pred)+len(ofs))
	for _, f := range ofs {
		out = append(out, Point{T: f.T, V: f.V, Forecast: true})
		info.ForecastSamples++
	}
	for _, p := range pred {
		if _, ok := ofsBy[p.T.Unix()]; ok {
			continue
		}
		out = append(out, Point{T: p.T, V: p.V + info.Offset, Forecast: false})
	}
	sortPoints(out)
	return out, info, nil
}

func markAll(in []Point, forecast bool) []Point {
	out := make([]Point, len(in))
	for i, p := range in {
		out[i] = Point{T: p.T, V: p.V, Forecast: forecast}
	}
	sortPoints(out)
	return out
}

func sortPoints(p []Point) {
	sort.SliceStable(p, func(i, j int) bool { return p[i].T.Before(p[j].T) })
}

// EstimatedPrefix is prepended to titles of events lacking forecast data.
const EstimatedPrefix = "*"

// FormatHeight renders a height like "6.9 ft".
func FormatHeight(v float64) string { return fmt.Sprintf("%.1f ft", v) }

// FormatClock renders a time like "10:06 AM".
func FormatClock(t time.Time) string { return t.Format("3:04 PM") }

// Title renders the event title from a template using {height}, {time} and
// {location} placeholders, prefixing EstimatedPrefix for estimated events.
func Title(ev Event, template, location string) string {
	r := strings.NewReplacer(
		"{height}", FormatHeight(ev.PeakHeight),
		"{time}", FormatClock(ev.PeakTime),
		"{location}", location,
	)
	t := r.Replace(template)
	if ev.Estimated {
		t = EstimatedPrefix + t
	}
	return t
}
