package tide

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

var la = mustLoc("America/Los_Angeles")

func mustLoc(name string) *time.Location {
	l, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return l
}

func ts(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, la)
	if err != nil {
		panic(err)
	}
	return t
}

// series builds 6-minute samples starting at start.
func series(start string, vals ...float64) []Point {
	t0 := ts(start)
	out := make([]Point, len(vals))
	for i, v := range vals {
		out[i] = Point{T: t0.Add(time.Duration(i) * 6 * time.Minute), V: v, Forecast: true}
	}
	return out
}

func TestInterpolateCrossing(t *testing.T) {
	t0 := ts("2026-01-01 10:00")
	t1 := ts("2026-01-01 10:06")
	got := interpolateCrossing(t0, 5.0, t1, 7.0, 6.0)
	if want := ts("2026-01-01 10:03"); !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if got := interpolateCrossing(t0, 6.0, t1, 6.0, 6.0); !got.Equal(t0) {
		t.Fatalf("flat line: got %v", got)
	}
}

func TestRoundToNearestTiesUp(t *testing.T) {
	step := 15 * time.Minute
	cases := map[string]string{
		"2026-01-01 10:07": "2026-01-01 10:00",
		"2026-01-01 10:08": "2026-01-01 10:15",
		"2026-01-01 10:22": "2026-01-01 10:15",
	}
	for in, want := range cases {
		if got := roundToNearest(ts(in), step, la); !got.Equal(ts(want)) {
			t.Errorf("%s: got %v want %s", in, got, want)
		}
	}
	tie := ts("2026-01-01 10:07").Add(30 * time.Second)
	if got := roundToNearest(tie, step, la); !got.Equal(ts("2026-01-01 10:15")) {
		t.Errorf("tie: got %v", got)
	}
}

func TestBuildEventsBasicWindow(t *testing.T) {
	pts := series("2026-01-01 10:00", 5, 7, 8, 7, 5)
	evs := BuildEvents(pts, 6, 30, 15, la)
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d", len(evs))
	}
	ev := evs[0]
	// crossings at 10:03 and 10:21 -> rounded 10:00 and 10:15
	if !ev.Start.Equal(ts("2026-01-01 10:00")) || !ev.End.Equal(ts("2026-01-01 10:15")) {
		t.Errorf("window %v - %v", ev.Start, ev.End)
	}
	if ev.PeakHeight != 8 || !ev.PeakTime.Equal(ts("2026-01-01 10:12")) {
		t.Errorf("peak %v @ %v", ev.PeakHeight, ev.PeakTime)
	}
	if ev.Estimated {
		t.Errorf("all-forecast window should not be estimated")
	}
}

func TestBuildEventsDegenerateWindowKeepsOneStep(t *testing.T) {
	pts := series("2026-01-01 10:00", 5.9, 6.05, 5.9)
	evs := BuildEvents(pts, 6, 30, 15, la)
	if len(evs) != 1 {
		t.Fatalf("got %d", len(evs))
	}
	if got := evs[0].End.Sub(evs[0].Start); got != 15*time.Minute {
		t.Errorf("duration %v", got)
	}
}

func TestBuildEventsMergesShortDip(t *testing.T) {
	// above, dip below for ~18 minutes, above again
	pts := series("2026-01-01 10:00", 7, 7, 5, 5, 5, 7, 7, 5)
	if evs := BuildEvents(pts, 6, 30, 15, la); len(evs) != 1 {
		t.Fatalf("merge-gap 30: got %d events", len(evs))
	}
	if evs := BuildEvents(pts, 6, 10, 15, la); len(evs) != 2 {
		t.Fatalf("merge-gap 10: got %d events", len(evs))
	}
}

func TestBuildEventsStartsAndEndsAboveThreshold(t *testing.T) {
	pts := series("2026-01-01 10:00", 7, 7, 7)
	evs := BuildEvents(pts, 6, 30, 15, la)
	if len(evs) != 1 || !evs[0].Start.Equal(ts("2026-01-01 10:00")) || !evs[0].End.Equal(ts("2026-01-01 10:15")) {
		t.Fatalf("got %+v", evs)
	}
}

func TestBuildEventsPeakTieKeepsFirst(t *testing.T) {
	pts := series("2026-01-01 10:00", 5, 7, 7, 5)
	evs := BuildEvents(pts, 6, 30, 15, la)
	if !evs[0].PeakTime.Equal(ts("2026-01-01 10:06")) {
		t.Fatalf("peak time %v", evs[0].PeakTime)
	}
}

func TestBuildEventsEstimatedWhenAnySampleNotForecast(t *testing.T) {
	pts := series("2026-01-01 10:00", 5, 7, 8, 7, 5)
	pts[3].Forecast = false
	evs := BuildEvents(pts, 6, 30, 15, la)
	if !evs[0].Estimated {
		t.Fatal("expected estimated")
	}
}

func TestBuildSeriesHybrid(t *testing.T) {
	pred := series("2026-01-01 10:00", 1, 2, 3, 4, 5)
	ofs := series("2026-01-01 10:00", 1.5, 2.5) // anomaly +0.5
	out, info, err := BuildSeries(pred, ofs, SourceHybrid, true)
	if err != nil {
		t.Fatal(err)
	}
	if info.OverlapSamples != 2 || math.Abs(info.Offset-0.5) > 1e-9 || !info.OffsetApplied {
		t.Fatalf("info %+v", info)
	}
	want := []Point{
		{V: 1.5, Forecast: true}, {V: 2.5, Forecast: true},
		{V: 3.5}, {V: 4.5}, {V: 5.5},
	}
	if len(out) != len(want) {
		t.Fatalf("len %d", len(out))
	}
	for i := range want {
		if math.Abs(out[i].V-want[i].V) > 1e-9 || out[i].Forecast != want[i].Forecast {
			t.Errorf("[%d] got %+v want %+v", i, out[i], want[i])
		}
		if !out[i].T.Equal(pred[i].T) {
			t.Errorf("[%d] time %v", i, out[i].T)
		}
	}
}

func TestBuildSeriesHybridNoOffset(t *testing.T) {
	pred := series("2026-01-01 10:00", 1, 2, 3)
	ofs := series("2026-01-01 10:00", 1.5)
	out, info, _ := BuildSeries(pred, ofs, SourceHybrid, false)
	if info.OffsetApplied || out[2].V != 3 {
		t.Fatalf("info %+v out %+v", info, out)
	}
}

func TestBuildSeriesHybridNoOFSFallsBackToTide(t *testing.T) {
	pred := series("2026-01-01 10:00", 1, 2)
	out, info, err := BuildSeries(pred, nil, SourceHybrid, true)
	if err != nil || info.OffsetApplied || len(out) != 2 || out[0].Forecast {
		t.Fatalf("out %+v info %+v err %v", out, info, err)
	}
}

func TestBuildSeriesForecastRequiresOFS(t *testing.T) {
	if _, _, err := BuildSeries(series("2026-01-01 10:00", 1), nil, SourceForecast, true); err == nil {
		t.Fatal("expected error")
	}
}

func TestBuildSeriesTideAllEstimated(t *testing.T) {
	out, _, _ := BuildSeries(series("2026-01-01 10:00", 1), series("2026-01-01 10:00", 9), SourceTide, true)
	if len(out) != 1 || out[0].Forecast || out[0].V != 1 {
		t.Fatalf("%+v", out)
	}
}

func TestTitle(t *testing.T) {
	ev := Event{PeakHeight: 6.94, PeakTime: ts("2026-01-01 10:06")}
	tmpl := "🌊 {height} @ {time} Possible Flooding ({location}) 🛟"
	if got, want := Title(ev, tmpl, "X"), "🌊 6.9 ft @ 10:06 AM Possible Flooding (X) 🛟"; got != want {
		t.Errorf("got %q", got)
	}
	ev.Estimated = true
	if got := Title(ev, tmpl, "X"); got != "*🌊 6.9 ft @ 10:06 AM Possible Flooding (X) 🛟" {
		t.Errorf("estimated: got %q", got)
	}
}

// --- golden parity against generate_flood_calendar.py -----------------------

type pyEvent struct {
	Start           string  `json:"start"`
	End             string  `json:"end"`
	DurationMinutes int     `json:"duration_minutes"`
	PeakHeightFt    float64 `json:"peak_height_ft"`
	PeakTime        string  `json:"peak_time"`
	Title           string  `json:"title"`
}

const pythonTitleTemplate = "\U0001F30A Possible Flooding ({location}) - High Tide {height} @ {time} \U0001F6DF"

func loadCachedPredictions(t *testing.T) []Point {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "predictions_lst_ldt_2026-11-09_2026-11-25.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Predictions []struct{ T, V string } `json:"predictions"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	pts := make([]Point, 0, len(doc.Predictions))
	for _, r := range doc.Predictions {
		v, err := strconv.ParseFloat(r.V, 64)
		if err != nil {
			t.Fatal(err)
		}
		pts = append(pts, Point{T: ts(r.T), V: v})
	}
	return pts
}

func TestGoldenParityWithPython(t *testing.T) {
	pts := loadCachedPredictions(t)
	for _, tc := range []struct {
		file      string
		minHeight float64
	}{
		{"python_events_6.0ft_2026-11-09_2026-11-25.json", 6.0},
		{"python_events_5.0ft_2026-11-09_2026-11-25.json", 5.0},
	} {
		t.Run(tc.file, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", tc.file))
			if err != nil {
				t.Fatal(err)
			}
			var want []pyEvent
			if err := json.Unmarshal(raw, &want); err != nil {
				t.Fatal(err)
			}
			got := BuildEvents(pts, tc.minHeight, 30, 15, la)
			if len(got) != len(want) {
				t.Fatalf("got %d events, python %d", len(got), len(want))
			}
			const iso = "2006-01-02T15:04:05-07:00"
			for i, w := range want {
				g := got[i]
				// Python titles have no estimate marker; compare the unmarked title.
				g.Estimated = false
				checks := []struct{ name, got, want string }{
					{"start", g.Start.Format(iso), w.Start},
					{"end", g.End.Format(iso), w.End},
					{"peak_time", g.PeakTime.Format(iso), w.PeakTime},
					{"title", Title(g, pythonTitleTemplate, "Manzanita/101 Highway"), w.Title},
				}
				for _, c := range checks {
					if c.got != c.want {
						t.Errorf("event %d %s: go %q python %q", i, c.name, c.got, c.want)
					}
				}
				if d := int(math.Round(g.End.Sub(g.Start).Minutes())); d != w.DurationMinutes {
					t.Errorf("event %d duration: go %d python %d", i, d, w.DurationMinutes)
				}
				if math.Abs(g.PeakHeight-w.PeakHeightFt) > 0.0051 {
					t.Errorf("event %d peak: go %v python %v", i, g.PeakHeight, w.PeakHeightFt)
				}
			}
		})
	}
}
