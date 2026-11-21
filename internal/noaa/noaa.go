// Package noaa fetches tide predictions and OFS water-level forecasts from the
// NOAA CO-OPS API.
package noaa

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/ragaskar/floodcal/internal/tide"
)

const DefaultEndpoint = "https://api.tidesandcurrents.noaa.gov/api/prod/datagetter"

// Product is a CO-OPS data product.
type Product string

const (
	ProductPredictions Product = "predictions"
	ProductOFS         Product = "ofs_water_level"
)

// Client talks to the CO-OPS datagetter endpoint.
type Client struct {
	Endpoint       string
	HTTP           *http.Client
	MaxRetries     int
	InitialBackoff time.Duration
	Sleep          func(time.Duration)
	Log            io.Writer
}

// NewClient returns a client with the same retry policy as the Python
// prototype (5 attempts, 2s doubling backoff).
func NewClient(log io.Writer) *Client {
	return &Client{
		Endpoint:       DefaultEndpoint,
		HTTP:           &http.Client{Timeout: 60 * time.Second},
		MaxRetries:     5,
		InitialBackoff: 2 * time.Second,
		Sleep:          time.Sleep,
		Log:            log,
	}
}

type row struct {
	T string `json:"t"`
	V string `json:"v"`
}

type response struct {
	Predictions []row `json:"predictions"`
	Data        []row `json:"data"`
	Error       *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Fetch returns 6-minute samples for [begin, end]. Times are requested and
// parsed in GMT, so DST transitions are unambiguous.
func (c *Client) Fetch(ctx context.Context, product Product, station, datum string, begin, end time.Time) ([]tide.Point, error) {
	q := url.Values{
		"station":    {station},
		"product":    {string(product)},
		"begin_date": {begin.UTC().Format("20060102 15:04")},
		"end_date":   {end.UTC().Format("20060102 15:04")},
		"datum":      {datum},
		"time_zone":  {"gmt"},
		"units":      {"english"},
		"format":     {"json"},
	}
	if product == ProductPredictions {
		q.Set("interval", "6")
	}
	u := c.Endpoint + "?" + q.Encode()

	body, err := c.getWithRetry(ctx, u, product)
	if err != nil {
		return nil, err
	}
	var resp response
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("NOAA %s: decoding response: %w", product, err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("NOAA %s API error: %s", product, resp.Error.Message)
	}
	rows := resp.Predictions
	if product == ProductOFS {
		rows = resp.Data
	}
	points := make([]tide.Point, 0, len(rows))
	for _, r := range rows {
		if r.V == "" {
			continue // missing value
		}
		t, err := time.ParseInLocation("2006-01-02 15:04", r.T, time.UTC)
		if err != nil {
			return nil, fmt.Errorf("NOAA %s: bad timestamp %q: %w", product, r.T, err)
		}
		v, err := strconv.ParseFloat(r.V, 64)
		if err != nil {
			return nil, fmt.Errorf("NOAA %s: bad value %q at %s: %w", product, r.V, r.T, err)
		}
		points = append(points, tide.Point{T: t, V: v, Forecast: product == ProductOFS})
	}
	return points, nil
}

func (c *Client) getWithRetry(ctx context.Context, u string, product Product) ([]byte, error) {
	backoff := c.InitialBackoff
	var lastErr error
	for attempt := 1; attempt <= c.MaxRetries; attempt++ {
		body, err := c.get(ctx, u)
		if err == nil {
			return body, nil
		}
		lastErr = err
		if attempt == c.MaxRetries || ctx.Err() != nil {
			break
		}
		fmt.Fprintf(c.Log, "[retry] NOAA %s request failed on attempt %d/%d: %v. Retrying in %s...\n",
			product, attempt, c.MaxRetries, err, backoff)
		c.Sleep(backoff)
		backoff *= 2
	}
	return nil, fmt.Errorf("NOAA %s request failed after %d attempts: %w", product, c.MaxRetries, lastErr)
}

func (c *Client) get(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "floodcal")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return body, nil
}
