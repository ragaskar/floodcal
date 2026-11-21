package noaa

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testClient(url string) *Client {
	c := NewClient(io.Discard)
	c.Endpoint = url
	c.Sleep = func(time.Duration) {}
	return c
}

func TestFetchPredictionsParsesGMT(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		io.WriteString(w, `{"predictions":[{"t":"2026-11-21 08:00","v":"2.338"},{"t":"2026-11-21 08:06","v":"2.4"}]}`)
	}))
	defer srv.Close()

	begin := time.Date(2026, 11, 21, 8, 0, 0, 0, time.UTC)
	pts, err := testClient(srv.URL).Fetch(context.Background(), ProductPredictions, "9414290", "MLLW", begin, begin.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 2 || !pts[0].T.Equal(begin) || pts[0].V != 2.338 || pts[0].Forecast {
		t.Fatalf("%+v", pts)
	}
	for _, want := range []string{"time_zone=gmt", "interval=6", "product=predictions", "begin_date=20261121+08%3A00"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query %q missing %q", gotQuery, want)
		}
	}
}

func TestFetchOFSUsesDataKeyAndSkipsBlanks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.RawQuery, "interval=") {
			t.Errorf("OFS request should not send interval")
		}
		io.WriteString(w, `{"metadata":{},"data":[{"t":"2026-11-21 08:00","v":"3.9"},{"t":"2026-11-21 08:06","v":""}]}`)
	}))
	defer srv.Close()
	pts, err := testClient(srv.URL).Fetch(context.Background(), ProductOFS, "s", "MLLW", time.Now(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 1 || !pts[0].Forecast {
		t.Fatalf("%+v", pts)
	}
}

func TestFetchAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"error":{"message":"No data was found"}}`)
	}))
	defer srv.Close()
	_, err := testClient(srv.URL).Fetch(context.Background(), ProductOFS, "s", "MLLW", time.Now(), time.Now())
	if err == nil || !strings.Contains(err.Error(), "No data was found") {
		t.Fatalf("err %v", err)
	}
}

func TestFetchRetriesThenSucceeds(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, `{"predictions":[]}`)
	}))
	defer srv.Close()
	var slept []time.Duration
	c := testClient(srv.URL)
	c.Sleep = func(d time.Duration) { slept = append(slept, d) }
	if _, err := c.Fetch(context.Background(), ProductPredictions, "s", "MLLW", time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if calls != 3 || len(slept) != 2 || slept[0] != 2*time.Second || slept[1] != 4*time.Second {
		t.Fatalf("calls %d slept %v", calls, slept)
	}
}

func TestFetchGivesUpAfterMaxRetries(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	if _, err := testClient(srv.URL).Fetch(context.Background(), ProductPredictions, "s", "MLLW", time.Now(), time.Now()); err == nil {
		t.Fatal("expected error")
	}
	if calls != 5 {
		t.Fatalf("calls %d", calls)
	}
}
