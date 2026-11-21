# Tam Valley Flood Calendar — Methodology

This file describes the `floodcal` Go CLI (current) and the original Python
prototype `generate_flood_calendar.py`, kept as a reference. Design decisions
are recorded in `docs/plan.md`.

## floodcal (Go CLI)

`floodcal` keeps a Google Calendar in sync with predicted flood windows for the
next N days (default 14). It is meant to run daily from cron:

```
floodcal --mode sync          # settings from ~/.config/floodcal/config.yaml
```

### What changed from the prototype

- **Heights come from the NOAA OFS forecast**
  (`product=ofs_water_level`, SFBOFS model guidance including wind, pressure
  and river effects) instead of only the astronomical tide table, which
  under-predicts flooding during weather events. OFS only reaches about 2–3
  days ahead (about 55 h at 9414290, checked 2026-11-21).
- **`--source hybrid` (default)**: OFS where available. After it ends, the tide
  table plus an **anomaly offset**: the mean of (OFS − tide table) over every
  timestamp both cover, applied as a constant to the rest of the window
  (`--anomaly-offset=false` to disable). `--source forecast` uses OFS only;
  `--source tide` reproduces the prototype.
- **`*` title prefix** marks any event whose window includes data that isn't from
  the OFS forecast (tide table, with or without offset). If OFS fails in hybrid
  mode, floodcal warns and falls back to the tide table, so every event is
  marked `*`.
- **Title** puts height/time first so truncated calendar titles stay useful:
  `🌊 6.9 ft @ 10:06 AM Possible Flooding (Manzanita/101 Highway) 🛟`.
- NOAA data is requested in **GMT** and converted locally, so the repeated
  daylight-saving hour is unambiguous. No caching: the forecast changes every
  model run.

### Google Calendar sync

- Auth: a service account JSON key (`service-account-keyfile`, falling back to
  `$GOOGLE_APPLICATION_CREDENTIALS`), scope `calendar.events` only. The target
  calendar (`calendar-id`) must be shared with the service account's email
  with "Make changes to events", and the project must have the Google Calendar
  API enabled.
- Events floodcal creates carry private extended properties (`floodcal=1`,
  station, peak time). Only those events are ever modified.
- Each run matches computed windows to existing events by **nearest peak
  within ±3 h**, then patches changed events, inserts new ones, and deletes
  ones no longer forecast. Only events whose peak falls within the time span
  NOAA actually returned are considered, so a short or partial fetch never
  deletes events beyond it.
- **Events that have started are frozen**: never patched or deleted. A
  window already in progress with no event yet is created once.
- `--dry-run` prints the planned changes. Each run prints a summary line
  (`created N, updated N, deleted N, unchanged N, frozen N`) and exits non-zero
  on any API error.

### Configuration

Every option can be set as a flag (`--min-height`), an env var
(`FLOODCAL_MIN_HEIGHT`) or a YAML key (`min-height`). Precedence: flag > env >
YAML > default. The YAML file defaults to `~/.config/floodcal/config.yaml`
(override with `--config` / `FLOODCAL_CONFIG`). See the annotated
`configs/example-config.yaml`. Unknown YAML keys are an error. Run
`floodcal --help` for the full list.

### Build

`make build` → `bin/floodcal`; `make test`; `make dist` →
`dist/floodcal_<version>_{darwin_arm64,linux_amd64,linux_arm64}.tar.gz` plus
`checksums.txt`. The version comes from `git describe --tags` (falls back to
`v0.0.0-dev`). Override the Go binary with `make GO=...`.

### Verification

`internal/tide` has a golden test asserting that the Go window logic
reproduces the Python prototype's output exactly on cached NOAA data
(2026-11-09..25, at 6.0 ft and 5.0 ft).

---

# Python prototype (reference)

How `tam-valley-flooding-*.ics` was built by `generate_flood_calendar.py`.

## What it does

Generates calendar events for every period when the NOAA-predicted tide at a
station is forecast to be at or above a threshold height — used here as a
proxy for likely flooding at the **Manzanita St / Highway 101 intersection in
Tam Valley**, which is known to flood around high tides of roughly 6ft.

## Data source

- **NOAA CO-OPS Tides & Currents API** (`api.tidesandcurrents.noaa.gov`), no
  API key required.
- **Station 9414290** (San Francisco) by default — configurable via
  `--station` for other flood-prone locations tied to a different gauge.
- **Product**: `predictions` (harmonic tide predictions), **not** observed
  water level. These are forecasts, not real-time readings.
- **Interval**: 6-minute (the finest resolution NOAA predictions offer).
- **Datum**: `MLLW` by default (NOAA's standard datum for published tide
  tables — this is almost certainly what "6ft" means in everyday tide talk,
  e.g. king tides quoted as ~7ft MLLW at this station). Configurable via
  `--datum`.
- **Time zone**: `America/Los_Angeles`, requested from NOAA as `lst_ldt`
  (local standard/daylight time) so timestamps already reflect PST/PDT
  civil clock time. Configurable via `--timezone`.

### Known limitation: predictions vs. reality

NOAA's predictions are the astronomical tide only. They don't include storm
surge, El Niño/seasonal sea-level anomalies, barometric pressure, wave
run-up, or rainfall/creek flow — all of which can make actual flooding occur
at a different predicted-tide height than in a typical year. This is exactly
why the script supports `--mode list`/`--mode json` with an adjustable
`--min-height`: re-run periodically against a short window and compare to
what's actually flooding, then retune the cutoff. See the sensitivity table
below for this year's starting point.

### Known limitation: DST transition hour

NOAA's `lst_ldt` feed doesn't disambiguate the repeated clock hour on the
November DST-end transition. Python's `zoneinfo` defaults to the first
(daylight) occurrence. This can only affect the displayed label of events
inside that single repeated hour once a year — not whether a flood window is
detected at all.

## Processing logic (`generate_flood_calendar.py`)

1. **Fetch** 6-minute predictions for the requested date range (default:
   today through one year later).
2. **Find raw crossings**: walk the sorted samples; wherever consecutive
   samples cross the threshold, **linearly interpolate** between them to
   estimate the precise crossing instant (rather than snapping to the
   nearest 6-minute sample).
3. **Merge near-adjacent windows**: if the tide dips below the threshold for
   **30 minutes or less** before rising back above it (a double-peak around
   the cutoff), treat it as one continuous event rather than two.
4. **Round** each window's start/end to the **nearest 15 minutes** (ties
   round up), per the assumption that the underlying prediction isn't
   perfectly precise anyway.
5. **Peak**: within each (merged) window, the highest individual 6-minute
   sample and its own timestamp — *not* rounded — are reported as the
   event's "high tide" value/time. This is the window's own local peak, not
   NOAA's separately-published daily high/low (HILO) tide, which could
   differ slightly in a double-peak situation.
6. **Title** format:
   `🌊 Possible Flooding (Manzanita/101 Highway) - High Tide {height} @ {time} 🛟`
   e.g. `🌊 Possible Flooding (Manzanita/101 Highway) - High Tide 6.9 ft @ 10:06 AM 🛟`
   — height to 1 decimal place, time as local 12-hour clock.
7. **Title**: fully template-able via `--title-template`, using `{height}`,
   `{time}`, and `{location}` placeholders — default shown above.
8. **Output**: `.ics` (hand-built VCALENDAR/VEVENT text, RFC5545 line-folded,
   DTSTART/DTEND stored in UTC for unambiguous Google Calendar import), a
   plain-text `list` for manual review, or structured `json`.

## Caching & API courtesy

Raw NOAA responses are cached to `.noaa_cache/noaa_<station>_<datum>_<tz>_<start>_<end>.json`
next to the script. If a matching cache file exists, it's used instead of
calling the API again, and a `[cache] ...` warning is printed to stderr.
**To force a fresh fetch, delete the relevant file under `.noaa_cache/`** (or
the whole directory to clear everything).

If the API call fails (network error, rate limiting, 5xx), the script
retries up to 5 times with exponential backoff (2s, 4s, 8s, 16s, 32s),
printing a `[retry] ...` message to stderr each time, before giving up.

## CLI reference

```
python3 generate_flood_calendar.py [options]

--station ID          NOAA station (default: 9414290)
--start YYYY-MM-DD     range start (default: today)
--end YYYY-MM-DD       range end (default: --start + 1 year)
--min-height FEET      alert threshold (default: 6.0 — see sensitivity table below)
--merge-gap MINUTES    merge windows separated by a dip of at most this long (default: 30)
--round-minutes MIN    round event start/end to nearest N minutes (default: 15)
--datum DATUM          NOAA tidal datum (default: MLLW)
--timezone TZ          IANA timezone (default: America/Los_Angeles)
--location-label TEXT  location name used in the event title (default: "Manzanita/101 Highway")
--title-template TEXT  event title template; placeholders: {height}, {time}, {location}
                       (default: "🌊 Possible Flooding ({location}) - High Tide {height} @ {time} 🛟")
--mode {ics,list,json} output format (default: list — stdout, for review before committing to a calendar file)
--output PATH          output file (default: auto-named for ics; stdout for list/json)
```

## How to regenerate for next year

`--start`/`--end` default to "today" and "today + 1 year", so running it
again next November rolls the window forward automatically. The default
mode (`list`) just prints to stdout for a quick review — pass `--mode ics`
to actually write the importable calendar file:

```
cd ~/Downloads/floodcal
python3 generate_flood_calendar.py                # eyeball the list first
python3 generate_flood_calendar.py --mode ics      # then write the .ics
```

Then import the resulting `.ics` into Google Calendar (Settings → Import &
export → Import).

## This year's run

Generated 2026-11-09 for the window **2026-11-09 to 2027-11-09**, station
**9414290**:

| min-height (ft MLLW) | events over full year | events in next 3 months* |
|---|---|---|
| 5.5  | —   | 74 |
| **6.0 (current default)** | **133** | **40** |
| 6.25 | —   | 23 |
| 6.5  | —   | 16 |
| 6.75 | —   | 11 |
| 7.0  | —   | 7  |

\* next 3 months = 2026-11-09 to 2027-02-09, the stretch with this year's
highest/most frequent tides — 40 events there (~1 every 2-3 days) is more
than it first looked once spread over the full year.

Current output file (6.0ft default): `tam-valley-flooding-2026-11-09-to-2027-11-09.ics`
(133 events).

**On tuning the cutoff**: the 3-month sensitivity check above shows how much
each 0.25ft of cutoff trims the count (40 at 6.0 → 23 at 6.25 → 16 at 6.5 →
7 at 7.0). Left at 6.0 for now — revisit against actual observed flooding at
Manzanita/101 and retune with `--min-height` (then regenerate the `.ics`)
once there's a season of real data to compare against.

Reproduce the sensitivity check with:
```
python3 generate_flood_calendar.py --mode json --min-height 6.5 --end 2027-02-09
```
