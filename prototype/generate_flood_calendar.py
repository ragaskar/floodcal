#!/usr/bin/env python3
"""
generate_flood_calendar.py

Fetches NOAA tide predictions for a station and generates either:
  - an .ics calendar of "high tide above threshold" windows, or
  - a plain-text list of those windows (for manual review / sensitivity checks)

Background / methodology: see METHODOLOGY.md in this directory.

Usage examples:
    # Default: 6ft threshold, next 12 months from today, prints a list to stdout
    python3 generate_flood_calendar.py

    # Write the actual Google Calendar import file instead
    python3 generate_flood_calendar.py --mode ics

    # Try a different cutoff, just for the next 3 months
    python3 generate_flood_calendar.py --min-height 6.5 --end 2027-02-09

    # Custom title wording (used by --mode ics and --mode json too)
    python3 generate_flood_calendar.py \\
        --title-template "FLOOD WATCH: {location} may hit {height} near {time}"
"""

import argparse
import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from datetime import datetime, timedelta, timezone
from zoneinfo import ZoneInfo

NOAA_ENDPOINT = "https://api.tidesandcurrents.noaa.gov/api/prod/datagetter"

DEFAULT_LOCATION_LABEL = "Manzanita/101 Highway"
# Placeholders available: {height}, {time}, {location}
DEFAULT_TITLE_TEMPLATE = "\U0001F30A Possible Flooding ({location}) - High Tide {height} @ {time} \U0001F6DF"

CACHE_DIR = os.path.join(os.path.dirname(os.path.abspath(__file__)), ".noaa_cache")
MAX_RETRIES = 5
INITIAL_BACKOFF_SECONDS = 2.0


def parse_args():
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--station", default="9414290", help="NOAA CO-OPS station ID (default: 9414290, San Francisco)")
    p.add_argument("--start", default=None, help="Start date YYYY-MM-DD (default: today)")
    p.add_argument("--end", default=None, help="End date YYYY-MM-DD (default: start + 1 year)")
    p.add_argument("--min-height", type=float, default=6.0, dest="min_height",
                   help="Minimum tide height in feet to alert on (default: 6.0)")
    p.add_argument("--merge-gap", type=int, default=30, dest="merge_gap_minutes",
                   help="Merge two windows separated by a dip of at most this many minutes (default: 30)")
    p.add_argument("--round-minutes", type=int, default=15, dest="round_minutes",
                   help="Round event start/end to the nearest N minutes (default: 15)")
    p.add_argument("--datum", default="MLLW", help="NOAA tidal datum (default: MLLW)")
    p.add_argument("--timezone", default="America/Los_Angeles", help="IANA timezone (default: America/Los_Angeles)")
    p.add_argument("--location-label", default=DEFAULT_LOCATION_LABEL, dest="location_label",
                   help=f"Location name used in event titles (default: '{DEFAULT_LOCATION_LABEL}')")
    p.add_argument("--title-template", default=DEFAULT_TITLE_TEMPLATE, dest="title_template",
                   help="Event title template. Placeholders: {height}, {time}, {location} "
                        f"(default: '{DEFAULT_TITLE_TEMPLATE}')")
    p.add_argument("--mode", choices=["ics", "list", "json"], default="list",
                   help="'list' prints a plain-text table for manual review (default); "
                        "'ics' writes a calendar file; 'json' prints structured event data")
    p.add_argument("--output", default=None, help="Output file path (default depends on --mode)")
    return p.parse_args()


def cache_path(station, start_date, end_date, datum, tz_name):
    safe_tz = tz_name.replace("/", "-")
    fname = f"noaa_{station}_{datum}_{safe_tz}_{start_date}_{end_date}.json"
    return os.path.join(CACHE_DIR, fname)


def fetch_raw_predictions(station, start_date, end_date, datum, tz_name):
    """Fetch (or load from cache) the raw NOAA predictions JSON for [start_date, end_date]."""
    path = cache_path(station, start_date, end_date, datum, tz_name)

    if os.path.exists(path):
        print(
            f"[cache] Using cached NOAA data from {path} instead of hitting the API. "
            f"To force a fresh fetch, delete this file and re-run.",
            file=sys.stderr,
        )
        with open(path, "r", encoding="utf-8") as f:
            return json.load(f)

    params = {
        "station": station,
        "begin_date": start_date.strftime("%Y%m%d"),
        "end_date": end_date.strftime("%Y%m%d"),
        "product": "predictions",
        "datum": datum,
        "time_zone": "lst_ldt",  # local standard/daylight time, matches civil clock time
        "units": "english",
        "interval": "6",
        "format": "json",
    }
    url = NOAA_ENDPOINT + "?" + urllib.parse.urlencode(params)

    backoff = INITIAL_BACKOFF_SECONDS
    last_err = None
    data = None
    for attempt in range(1, MAX_RETRIES + 1):
        try:
            with urllib.request.urlopen(url, timeout=60) as resp:
                data = json.load(resp)
            last_err = None
            break
        except (urllib.error.HTTPError, urllib.error.URLError, TimeoutError) as e:
            last_err = e
            status = getattr(e, "code", None)
            if attempt == MAX_RETRIES:
                break
            print(
                f"[retry] NOAA API request failed on attempt {attempt}/{MAX_RETRIES}"
                f"{f' (HTTP {status})' if status else ''}: {e}. Retrying in {backoff:.0f}s...",
                file=sys.stderr,
            )
            time.sleep(backoff)
            backoff *= 2

    if last_err is not None:
        raise RuntimeError(f"NOAA API request failed after {MAX_RETRIES} attempts: {last_err}") from last_err

    if "error" in data:
        raise RuntimeError(f"NOAA API error: {data['error']}")

    os.makedirs(CACHE_DIR, exist_ok=True)
    with open(path, "w", encoding="utf-8") as f:
        json.dump(data, f)

    return data


def fetch_predictions(station, start_date, end_date, datum, tz_name):
    """Fetch 6-minute interval tide predictions from NOAA CO-OPS for [start_date, end_date]."""
    data = fetch_raw_predictions(station, start_date, end_date, datum, tz_name)

    tz = ZoneInfo(tz_name)
    points = []
    for row in data["predictions"]:
        # "t" is local wall-clock time (lst_ldt), naive; attach tzinfo directly.
        # NOTE: during the one hour repeated each November DST-end, Python's
        # zoneinfo defaults to the first (daylight) occurrence (fold=0). NOAA's
        # lst_ldt feed does not disambiguate either. This can only affect
        # labeling within that single repeated hour, not detection.
        naive = datetime.strptime(row["t"], "%Y-%m-%d %H:%M")
        dt = naive.replace(tzinfo=tz)
        points.append((dt, float(row["v"])))
    points.sort(key=lambda p: p[0])
    return points


def interpolate_crossing_time(t0, v0, t1, v1, threshold):
    """Linearly interpolate the datetime at which the line through (t0,v0)-(t1,v1) == threshold."""
    if v1 == v0:
        return t0
    frac = (threshold - v0) / (v1 - v0)
    frac = min(1.0, max(0.0, frac))
    delta = (t1 - t0) * frac
    return t0 + delta


def find_raw_windows(points, threshold):
    """Find raw (unmerged, unrounded) above-threshold windows with their constituent samples."""
    windows = []
    current = None  # dict(start, end, samples=[(dt,v), ...])

    n = len(points)
    for i in range(n):
        dt, v = points[i]
        above = v >= threshold

        if above and current is None:
            # Started a new window. Determine precise start.
            if i == 0:
                start = dt  # data begins already above threshold; use first sample as-is
            else:
                pdt, pv = points[i - 1]
                start = interpolate_crossing_time(pdt, pv, dt, v, threshold)
            current = {"start": start, "end": None, "samples": [(dt, v)]}
        elif above and current is not None:
            current["samples"].append((dt, v))
        elif not above and current is not None:
            # Just dropped below threshold; determine precise end using previous sample.
            pdt, pv = points[i - 1]
            end = interpolate_crossing_time(pdt, pv, dt, v, threshold)
            current["end"] = end
            windows.append(current)
            current = None

    if current is not None:
        # Data ends while still above threshold.
        current["end"] = points[-1][0]
        windows.append(current)

    return windows


def merge_windows(windows, merge_gap_minutes):
    if not windows:
        return []
    merge_gap = timedelta(minutes=merge_gap_minutes)
    merged = [dict(windows[0])]
    merged[-1]["samples"] = list(windows[0]["samples"])

    for w in windows[1:]:
        gap = w["start"] - merged[-1]["end"]
        if gap <= merge_gap:
            merged[-1]["end"] = w["end"]
            merged[-1]["samples"].extend(w["samples"])
        else:
            merged.append(dict(w))
            merged[-1]["samples"] = list(w["samples"])
    return merged


def round_to_nearest(dt, minutes):
    """Round a tz-aware datetime to the nearest `minutes` (ties round up)."""
    epoch = datetime(1970, 1, 1, tzinfo=dt.tzinfo)
    total_seconds = (dt - epoch).total_seconds()
    step = minutes * 60
    rounded = (total_seconds + step / 2) // step * step  # ties round up
    return epoch + timedelta(seconds=rounded)


def build_events(points, min_height, merge_gap_minutes, round_minutes):
    raw = find_raw_windows(points, min_height)
    merged = merge_windows(raw, merge_gap_minutes)

    events = []
    for w in merged:
        start_r = round_to_nearest(w["start"], round_minutes)
        end_r = round_to_nearest(w["end"], round_minutes)
        if end_r <= start_r:
            # Degenerate after rounding (very short spike); keep at least one interval.
            end_r = start_r + timedelta(minutes=round_minutes)
        peak_dt, peak_v = max(w["samples"], key=lambda s: s[1])
        events.append({
            "start": start_r,
            "end": end_r,
            "peak_time": peak_dt,
            "peak_height": peak_v,
        })
    return events


def format_height(v):
    return f"{v:.1f} ft"


def format_clock(dt):
    s = dt.strftime("%I:%M %p").lstrip("0")
    return s


def event_title(ev, location_label=DEFAULT_LOCATION_LABEL, title_template=DEFAULT_TITLE_TEMPLATE):
    return title_template.format(
        height=format_height(ev["peak_height"]),
        time=format_clock(ev["peak_time"]),
        location=location_label,
    )


# --- ICS generation --------------------------------------------------------

def ics_escape(text):
    return (text.replace("\\", "\\\\")
                .replace(";", "\\;")
                .replace(",", "\\,")
                .replace("\n", "\\n"))


def fold_ics_line(line, limit=75):
    """RFC5545 line folding: continuation lines start with a single space."""
    encoded = line.encode("utf-8")
    if len(encoded) <= limit:
        return line
    parts = []
    start = 0
    first = True
    while start < len(encoded):
        chunk_limit = limit if first else limit - 1
        end = min(start + chunk_limit, len(encoded))
        # avoid splitting a multi-byte utf-8 sequence
        while end > start:
            try:
                chunk = encoded[start:end].decode("utf-8")
                break
            except UnicodeDecodeError:
                end -= 1
        else:
            chunk = ""
        parts.append(chunk if first else " " + chunk)
        start = end
        first = False
    return "\r\n".join(parts)


def write_ics(events, station, min_height, datum, tz_name, output_path, location_label=DEFAULT_LOCATION_LABEL,
              title_template=DEFAULT_TITLE_TEMPLATE):
    now_utc = datetime.now(timezone.utc)
    lines = [
        "BEGIN:VCALENDAR",
        "VERSION:2.0",
        "PRODID:-//floodcal//Tam Valley Flood Calendar//EN",
        "CALSCALE:GREGORIAN",
        "METHOD:PUBLISH",
    ]
    for i, ev in enumerate(events):
        start_utc = ev["start"].astimezone(timezone.utc)
        end_utc = ev["end"].astimezone(timezone.utc)
        title = event_title(ev, location_label, title_template)
        description = (
            f"Predicted tide at or above {min_height:.1f} ft at NOAA station {station} "
            f"(datum: {datum}). Window: {ev['start'].strftime('%Y-%m-%d %I:%M %p')} - "
            f"{ev['end'].strftime('%Y-%m-%d %I:%M %p')} ({tz_name}). "
            f"Peak: {format_height(ev['peak_height'])} at {ev['peak_time'].strftime('%Y-%m-%d %I:%M %p')}. "
            f"Source: https://tidesandcurrents.noaa.gov/stationhome.html?id={station}"
        )
        uid = f"floodcal-{station}-{start_utc.strftime('%Y%m%dT%H%M%SZ')}-{i}@floodcal.local"
        lines.append("BEGIN:VEVENT")
        lines.append(fold_ics_line(f"UID:{uid}"))
        lines.append(fold_ics_line(f"DTSTAMP:{now_utc.strftime('%Y%m%dT%H%M%SZ')}"))
        lines.append(fold_ics_line(f"DTSTART:{start_utc.strftime('%Y%m%dT%H%M%SZ')}"))
        lines.append(fold_ics_line(f"DTEND:{end_utc.strftime('%Y%m%dT%H%M%SZ')}"))
        lines.append(fold_ics_line(f"SUMMARY:{ics_escape(title)}"))
        lines.append(fold_ics_line(f"DESCRIPTION:{ics_escape(description)}"))
        lines.append("END:VEVENT")
    lines.append("END:VCALENDAR")

    # newline="" disables Python's newline translation so the \r\n we already
    # inserted for RFC5545 folding isn't double-translated into \r\r\n.
    with open(output_path, "w", encoding="utf-8", newline="") as f:
        f.write("\r\n".join(lines) + "\r\n")


def write_list(events, output_path, location_label=DEFAULT_LOCATION_LABEL, title_template=DEFAULT_TITLE_TEMPLATE):
    header = f"{'start':<22} {'end':<22} {'dur(min)':>8} {'peak_ft':>8} {'peak_time':<22} title"
    rows = [header, "-" * len(header)]
    for ev in events:
        dur_min = int(round((ev["end"] - ev["start"]).total_seconds() / 60))
        rows.append(
            f"{ev['start'].strftime('%Y-%m-%d %I:%M %p'):<22} "
            f"{ev['end'].strftime('%Y-%m-%d %I:%M %p'):<22} "
            f"{dur_min:>8} "
            f"{ev['peak_height']:>8.2f} "
            f"{ev['peak_time'].strftime('%Y-%m-%d %I:%M %p'):<22} "
            f"{event_title(ev, location_label, title_template)}"
        )
    text = "\n".join(rows) + "\n"
    if output_path:
        with open(output_path, "w", encoding="utf-8") as f:
            f.write(text)
    else:
        sys.stdout.write(text)


def write_json(events, output_path, location_label=DEFAULT_LOCATION_LABEL, title_template=DEFAULT_TITLE_TEMPLATE):
    payload = []
    for ev in events:
        payload.append({
            "start": ev["start"].isoformat(),
            "end": ev["end"].isoformat(),
            "duration_minutes": int(round((ev["end"] - ev["start"]).total_seconds() / 60)),
            "peak_height_ft": round(ev["peak_height"], 2),
            "peak_time": ev["peak_time"].isoformat(),
            "title": event_title(ev, location_label, title_template),
        })
    text = json.dumps(payload, indent=2) + "\n"
    if output_path:
        with open(output_path, "w", encoding="utf-8") as f:
            f.write(text)
    else:
        sys.stdout.write(text)


def main():
    args = parse_args()

    tz = ZoneInfo(args.timezone)
    start_date = (datetime.strptime(args.start, "%Y-%m-%d").date()
                  if args.start else datetime.now(tz).date())
    end_date = (datetime.strptime(args.end, "%Y-%m-%d").date()
                if args.end else start_date.replace(year=start_date.year + 1))

    points = fetch_predictions(args.station, start_date, end_date, args.datum, args.timezone)
    events = build_events(points, args.min_height, args.merge_gap_minutes, args.round_minutes)

    if args.mode == "ics":
        output_path = args.output or f"tam-valley-flooding-{start_date}-to-{end_date}.ics"
        write_ics(events, args.station, args.min_height, args.datum, args.timezone, output_path,
                   location_label=args.location_label, title_template=args.title_template)
        print(f"Wrote {len(events)} events to {output_path}")
    elif args.mode == "json":
        write_json(events, args.output, location_label=args.location_label, title_template=args.title_template)
        if args.output:
            print(f"Wrote {len(events)} events to {args.output}", file=sys.stderr)
    else:
        write_list(events, args.output, location_label=args.location_label, title_template=args.title_template)
        if args.output:
            print(f"Wrote {len(events)} events to {args.output}", file=sys.stderr)


if __name__ == "__main__":
    main()
