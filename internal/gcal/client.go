package gcal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"golang.org/x/oauth2/google"
	"google.golang.org/api/calendar/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// Private extended property keys used to tag and match floodcal events.
const (
	propTag     = "floodcal"
	propStation = "floodcal-station"
	propPeak    = "floodcal-peak"
)

// API is the subset of Google Calendar operations sync needs.
type API interface {
	List(ctx context.Context, station string, from, to time.Time) ([]Existing, error)
	Insert(ctx context.Context, station string, d Desired) error
	Patch(ctx context.Context, station, id string, d Desired) error
	Delete(ctx context.Context, id string) error
}

// Client implements API against a real calendar.
type Client struct {
	svc        *calendar.Service
	calendarID string
	timezone   string
	log        io.Writer
	retries    int
	backoff    time.Duration
	sleep      func(time.Duration)
	// pageSize is the events.list page size (Google's max is 2500; 250 is
	// its default). Tests lower it to exercise pagination cheaply.
	pageSize int64
}

// NewClient authenticates with a service account key file, requesting only
// the calendar.events scope.
func NewClient(ctx context.Context, keyfile, calendarID, timezone string, log io.Writer) (*Client, error) {
	key, err := os.ReadFile(keyfile)
	if err != nil {
		return nil, fmt.Errorf("reading service account keyfile: %w", err)
	}
	creds, err := google.CredentialsFromJSON(ctx, key, calendar.CalendarEventsScope)
	if err != nil {
		return nil, fmt.Errorf("parsing service account keyfile %s: %w", keyfile, err)
	}
	svc, err := calendar.NewService(ctx, option.WithCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("creating calendar client: %w", err)
	}
	return &Client{svc: svc, calendarID: calendarID, timezone: timezone, log: log,
		retries: 5, backoff: 2 * time.Second, sleep: time.Sleep, pageSize: 250}, nil
}

func retryable(err error) bool {
	var gerr *googleapi.Error
	if errors.As(err, &gerr) {
		if gerr.Code == http.StatusTooManyRequests || gerr.Code >= 500 {
			return true
		}
		// Calendar reports rate limiting as 403 with these reasons.
		for _, e := range gerr.Errors {
			if e.Reason == "rateLimitExceeded" || e.Reason == "userRateLimitExceeded" {
				return true
			}
		}
	}
	return false
}

func (c *Client) do(what string, fn func() error) error {
	backoff := c.backoff
	for attempt := 1; ; attempt++ {
		err := fn()
		if err == nil {
			return nil
		}
		if attempt == c.retries || !retryable(err) {
			return fmt.Errorf("google calendar %s: %w", what, err)
		}
		fmt.Fprintf(c.log, "[retry] google calendar %s failed on attempt %d/%d: %v. Retrying in %s...\n",
			what, attempt, c.retries, err, backoff)
		c.sleep(backoff)
		backoff *= 2
	}
}

// List returns floodcal events for station overlapping [from, to].
func (c *Client) List(ctx context.Context, station string, from, to time.Time) ([]Existing, error) {
	var out []Existing
	pageToken := ""
	for {
		var resp *calendar.Events
		err := c.do("list events", func() error {
			call := c.svc.Events.List(c.calendarID).Context(ctx).
				PrivateExtendedProperty(propTag+"=1").
				PrivateExtendedProperty(propStation+"="+station).
				TimeMin(from.Format(time.RFC3339)).
				TimeMax(to.Format(time.RFC3339)).
				SingleEvents(true).
				ShowDeleted(false).
				MaxResults(c.pageSize)
			if pageToken != "" {
				call = call.PageToken(pageToken)
			}
			var err error
			resp, err = call.Do()
			return err
		})
		if err != nil {
			return nil, err
		}
		for _, ev := range resp.Items {
			e, err := toExisting(ev)
			if err != nil {
				return nil, err
			}
			out = append(out, e)
		}
		if resp.NextPageToken == "" {
			return out, nil
		}
		pageToken = resp.NextPageToken
	}
}

func toExisting(ev *calendar.Event) (Existing, error) {
	if ev.Start == nil || ev.End == nil {
		return Existing{}, fmt.Errorf("event %s has no start/end", ev.Id)
	}
	start, err := time.Parse(time.RFC3339, ev.Start.DateTime)
	if err != nil {
		return Existing{}, fmt.Errorf("event %s start %q: %w", ev.Id, ev.Start.DateTime, err)
	}
	end, err := time.Parse(time.RFC3339, ev.End.DateTime)
	if err != nil {
		return Existing{}, fmt.Errorf("event %s end %q: %w", ev.Id, ev.End.DateTime, err)
	}
	peak := start.Add(end.Sub(start) / 2)
	if ev.ExtendedProperties != nil {
		if p, ok := ev.ExtendedProperties.Private[propPeak]; ok {
			if t, err := time.Parse(time.RFC3339, p); err == nil {
				peak = t
			}
		}
	}
	return Existing{ID: ev.Id, Start: start, End: end, PeakTime: peak, Title: ev.Summary, Description: ev.Description}, nil
}

func (c *Client) body(station string, d Desired) *calendar.Event {
	return &calendar.Event{
		Summary:     d.Title,
		Description: d.Description,
		Start:       &calendar.EventDateTime{DateTime: d.Start.Format(time.RFC3339), TimeZone: c.timezone},
		End:         &calendar.EventDateTime{DateTime: d.End.Format(time.RFC3339), TimeZone: c.timezone},
		ExtendedProperties: &calendar.EventExtendedProperties{Private: map[string]string{
			propTag:     "1",
			propStation: station,
			propPeak:    d.PeakTime.UTC().Format(time.RFC3339),
		}},
	}
}

func (c *Client) Insert(ctx context.Context, station string, d Desired) error {
	return c.do("insert event", func() error {
		_, err := c.svc.Events.Insert(c.calendarID, c.body(station, d)).Context(ctx).Do()
		return err
	})
}

func (c *Client) Patch(ctx context.Context, station, id string, d Desired) error {
	return c.do("patch event "+id, func() error {
		_, err := c.svc.Events.Patch(c.calendarID, id, c.body(station, d)).Context(ctx).Do()
		return err
	})
}

func (c *Client) Delete(ctx context.Context, id string) error {
	err := c.do("delete event "+id, func() error {
		return c.svc.Events.Delete(c.calendarID, id).Context(ctx).Do()
	})
	var gerr *googleapi.Error
	if errors.As(err, &gerr) && (gerr.Code == http.StatusGone || gerr.Code == http.StatusNotFound) {
		fmt.Fprintf(c.log, "[sync] event %s was already deleted\n", id)
		return nil
	}
	return err
}
