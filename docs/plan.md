# floodcal (Go) — Implementation Plan

Status: **APPROVED 2026-11-21.** Decisions recorded below. Earlier option tables were removed once each choice was made.

## Goal

Port `generate_flood_calendar.py` into a Go CLI called `floodcal`. Its main job is a **daily cron run that keeps a
Google Calendar in sync** with flood windows for the next N days. It uses **forecast water levels** (which include
weather) instead of the astronomical tide table alone. The `list`, `json` and `ics` outputs stay available for
debugging and manual workflows.

## Decisions

| ID | Topic | Decision |
|---|---|---|
| — | Binary / module | `floodcal`, module `github.com/ragaskar/floodcal` |
| — | Platforms | darwin/arm64, linux/amd64, linux/arm64 |
| — | Make targets | `make build` → `./bin`, `make dist` → versioned archives in `./dist`, `make test` |
| — | Config sources | flags, env vars (`FLOODCAL_*`), YAML (default `~/.config/floodcal/config.yaml`, override with `--config` / `FLOODCAL_CONFIG`). Every option except `--config`/`--version` can be set in YAML. Precedence: flag > env > yaml > default. |
| — | YAML key style | kebab-case, same as flag names. Unknown keys are an error. |
| — | Example config | `configs/example-config.yaml` |
| — | Calendar | Google Calendar only, selected by `calendar-id` (no lookup by name) |
| — | Auth | Service account JSON (`service-account-keyfile`), scope `calendar.events` only |
| S0 | Credential fallback | If no keyfile is configured, use `GOOGLE_APPLICATION_CREDENTIALS` |
| A1 | Anomaly | Mean of (OFS − tide table) over every timestamp both cover |
| A2 | Offset after OFS ends | That mean, applied as a constant. No decay, no clamping. |
| F3 | `--anomaly-offset` | Kept (default `true`). `false` uses the plain tide table after OFS ends. |
| F1 | Window | `--days` only (default 14). No `--end`. |
| C1 | Commands | Single command, `--mode list\|json\|ics\|sync`, default `list` |
| F2 | Config library | `spf13/pflag` + `gopkg.in/yaml.v3` + a hand-written merge step |
| K1 | Caching | No caching. Both NOAA products are fetched fresh each run (the Python cache key included the start date, so a daily cron run would never get a hit). |
| T1 | Title | No `{source}` placeholder; `{estimated}` placeholder added later |
| S1 | Matching | Existing event matches the computed window with the nearest peak within ±3 h (peak stored in a private extended property) |
| S2 | Ongoing events | Frozen once started: never patched or deleted. A window already in progress with no event gets created once. |
| — | Stale events | Deleted (only floodcal-tagged, not-yet-started events, and only within the time span the fetched data covers) |
| M1 | Makefile Go | `GO ?= go` |
| — | Timestamps | NOAA data requested in GMT and converted in Go. This avoids the repeated-hour ambiguity when daylight saving ends. |
| — | OFS missing/failed (hybrid) | Fall back: warn on stderr and fill with tide table (+offset if available). |
| — | Estimate marker | Any event whose window includes a timestamp without OFS forecast data is "estimated". Title placeholder `{estimated}` renders as `estimated-label` (default `*`) for those events and as nothing otherwise. Default template ends with ` {estimated}`. A custom template without it shows no marker. Applies to every source (all `tide` events are estimated). |
| — | `--source forecast` + OFS failure | Error (no fallback: that mode means forecast data only). |

## Data sources

- **CO-OPS `product=predictions`**: astronomical tide table, 6-minute intervals.
- **CO-OPS `product=ofs_water_level`**: NOS OFS (SFBOFS) model guidance including weather, 6-minute intervals.
  About 55 h ahead for 9414290 (checked 2026-11-21). The response uses a `data` key instead of `predictions`.
- Out of scope: ETSS / STOFS ingestion for days 3–7.

## Series construction (`--source`)

- `tide`: tide table only (all estimated).
- `forecast`: OFS only.
- `hybrid` (default): at each timestamp, use OFS if available. Otherwise use tide table + offset (offset = 0 when
  `--anomaly-offset=false` or there's no overlap), marked estimated.

Then: window detection with linear interpolation of threshold crossings, merging (`--merge-gap`), rounding
(`--round-minutes`, ties round up), peak = highest sample (first on ties). All ported exactly from the Python
script.

## Google Calendar sync

1. Data range = first to last timestamp of the fetched series.
2. List floodcal-tagged events for this station in that range (all pages). Ignore events whose peak falls
   outside the range.
3. Match computed windows to existing events by nearest peak within ±3 h.
4. Matched + frozen (already started) → leave alone. Matched + changed → patch. Matched + same → no API call.
   Unmatched computed (not yet ended) → insert. Unmatched existing (not frozen) → delete. Extra duplicates → delete.
5. Change detection compares start/end times, title and description. The description is deterministic
   (no run timestamp in it).
6. No custom event IDs (Google won't reuse the ID of a deleted event, so a later insert would fail).
7. Retries with backoff on 429/5xx. `--dry-run` prints the planned changes. Prints a one-line summary for cron logs.
   Exits non-zero on any API failure.

## Flags / config keys

| Flag | Default |
|---|---|
| `--config` | `~/.config/floodcal/config.yaml` (CLI/env only) |
| `--station` | `9414290` |
| `--start` | today in `--timezone` |
| `--days` | `14` |
| `--min-height` | `6.0` |
| `--merge-gap` | `30` |
| `--round-minutes` | `15` |
| `--datum` | `MLLW` |
| `--timezone` | `America/Los_Angeles` |
| `--location-label` | `Manzanita/101 Highway` |
| `--title-template` | `🌊 {height} @ {time} Possible Flooding ({location}) 🛟 {estimated}` |
| `--estimated-label` | `*` |
| `--mode` | `list` |
| `--output` | stdout (ics: auto-named) |
| `--source` | `hybrid` |
| `--anomaly-offset` | `true` |
| `--calendar-id` | — (required for sync) |
| `--service-account-keyfile` | — (falls back to `GOOGLE_APPLICATION_CREDENTIALS`) |
| `--dry-run` | `false` |
| `--version` | (CLI only) |

## Repository layout

```
cmd/floodcal/main.go
internal/config/   internal/noaa/   internal/tide/   internal/output/   internal/gcal/
configs/example-config.yaml
docs/plan.md, METHODOLOGY.md
Makefile, go.mod
```

## Makefile

- `VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo v0.0.0-dev)` → `-X main.version`.
- `build` → `bin/floodcal`. `dist` → `dist/floodcal_$(VERSION)_<os>_<arch>.tar.gz` ×3 + `checksums.txt`
  (`CGO_ENABLED=0`). `test` → `go test ./...`. `clean`.

## Verification

1. Golden parity: Go tide-only events on the cached 2026-11-09..25 NOAA data match the Python JSON output.
2. Config coverage tests: every option has a flag, an env var and a YAML key. The example config's keys exactly
   match the option list.
3. Sync planner unit tests: drift updates, disappearance deletes, new inserts, frozen untouched, duplicates cleaned.
4. Live on `flood-cal-test`: dry-run → sync → second sync reports 0 created / 0 deleted.

## Cron example

```
15 5 * * * /usr/local/bin/floodcal --mode sync >> ~/.local/state/floodcal.log 2>&1
```
