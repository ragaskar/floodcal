// Package config loads floodcal settings from (highest precedence first)
// command-line flags, FLOODCAL_* environment variables, a YAML config file,
// and built-in defaults. Every option is defined once in the options table and
// the flag name, YAML key and env var are derived from it.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/pflag"
	"gopkg.in/yaml.v3"
)

const (
	DefaultTitleTemplate = "\U0001F30A {height} @ {time} Possible Flooding ({location}) \U0001F6DF"
	EnvPrefix            = "FLOODCAL_"
	ConfigEnv            = EnvPrefix + "CONFIG"
	GoogleCredentialsEnv = "GOOGLE_APPLICATION_CREDENTIALS"
)

// Config is the fully resolved configuration.
type Config struct {
	Station               string
	Start                 string
	Days                  int
	MinHeight             float64
	MergeGap              int
	RoundMinutes          int
	Datum                 string
	Timezone              string
	LocationLabel         string
	TitleTemplate         string
	Mode                  string
	Output                string
	Source                string
	AnomalyOffset         bool
	CalendarID            string
	ServiceAccountKeyfile string
	DryRun                bool

	// ConfigPath is the config file that was loaded ("" if none).
	ConfigPath string
	// Location is the parsed Timezone.
	Location *time.Location
	// StartDate is local midnight of the first day of the window.
	StartDate time.Time
}

type kind int

const (
	kindString kind = iota
	kindInt
	kindFloat
	kindBool
)

type option struct {
	name string // flag name and YAML key (kebab-case)
	kind kind
	def  string
	help string
	set  func(c *Config, v string) error
}

// EnvName returns the environment variable for an option name.
func EnvName(name string) string {
	return EnvPrefix + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
}

func str(p func(*Config) *string) func(*Config, string) error {
	return func(c *Config, v string) error { *p(c) = v; return nil }
}

func integer(p func(*Config) *int) func(*Config, string) error {
	return func(c *Config, v string) error {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return fmt.Errorf("not an integer: %q", v)
		}
		*p(c) = n
		return nil
	}
}

func float(p func(*Config) *float64) func(*Config, string) error {
	return func(c *Config, v string) error {
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return fmt.Errorf("not a number: %q", v)
		}
		*p(c) = f
		return nil
	}
}

func boolean(p func(*Config) *bool) func(*Config, string) error {
	return func(c *Config, v string) error {
		b, err := strconv.ParseBool(strings.TrimSpace(v))
		if err != nil {
			return fmt.Errorf("not a boolean: %q", v)
		}
		*p(c) = b
		return nil
	}
}

// options is the single source of truth for every configurable setting.
var options = []option{
	{"station", kindString, "9414290", "NOAA CO-OPS station ID (9414290 = San Francisco)", str(func(c *Config) *string { return &c.Station })},
	{"start", kindString, "", "first day of the window, YYYY-MM-DD (default: today in --timezone)", str(func(c *Config) *string { return &c.Start })},
	{"days", kindInt, "14", "number of days in the window", integer(func(c *Config) *int { return &c.Days })},
	{"min-height", kindFloat, "6.0", "minimum water level in feet to alert on", float(func(c *Config) *float64 { return &c.MinHeight })},
	{"merge-gap", kindInt, "30", "merge windows separated by a dip of at most this many minutes", integer(func(c *Config) *int { return &c.MergeGap })},
	{"round-minutes", kindInt, "15", "round event start/end to the nearest N minutes", integer(func(c *Config) *int { return &c.RoundMinutes })},
	{"datum", kindString, "MLLW", "NOAA tidal datum", str(func(c *Config) *string { return &c.Datum })},
	{"timezone", kindString, "America/Los_Angeles", "IANA timezone for event times", str(func(c *Config) *string { return &c.Timezone })},
	{"location-label", kindString, "Manzanita/101 Highway", "location name used in event titles", str(func(c *Config) *string { return &c.LocationLabel })},
	{"title-template", kindString, DefaultTitleTemplate, "event title template; placeholders {height}, {time}, {location}", str(func(c *Config) *string { return &c.TitleTemplate })},
	{"mode", kindString, "list", "list | json | ics | sync (sync updates Google Calendar)", str(func(c *Config) *string { return &c.Mode })},
	{"output", kindString, "", "output file for list/json/ics (default: stdout; ics auto-named)", str(func(c *Config) *string { return &c.Output })},
	{"source", kindString, "hybrid", "hybrid | forecast | tide: hybrid = OFS forecast, then tide table (+anomaly offset) after it ends", str(func(c *Config) *string { return &c.Source })},
	{"anomaly-offset", kindBool, "true", "hybrid only: add mean (forecast - tide table) to tide-table heights after the forecast ends", boolean(func(c *Config) *bool { return &c.AnomalyOffset })},
	{"calendar-id", kindString, "", "Google Calendar ID to sync (Settings > Integrate calendar)", str(func(c *Config) *string { return &c.CalendarID })},
	{"service-account-keyfile", kindString, "", "path to the Google service account JSON key (falls back to $" + GoogleCredentialsEnv + ")", str(func(c *Config) *string { return &c.ServiceAccountKeyfile })},
	{"dry-run", kindBool, "false", "sync only: print planned calendar changes without making them", boolean(func(c *Config) *bool { return &c.DryRun })},
}

// OptionNames lists every option that may appear in the YAML config.
func OptionNames() []string {
	out := make([]string, len(options))
	for i, o := range options {
		out[i] = o.name
	}
	return out
}

// DefaultConfigPath is ~/.config/floodcal/config.yaml.
func DefaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "floodcal", "config.yaml")
}

// ErrHelp is returned when --help was requested.
var ErrHelp = pflag.ErrHelp

// Result carries parse outcomes beyond the config itself.
type Result struct {
	Config      *Config
	ShowVersion bool
}

// Load resolves configuration from args (without program name), env lookup
// and the filesystem. usage receives --help output.
func Load(args []string, lookupEnv func(string) (string, bool), usage io.Writer, now time.Time) (*Result, error) {
	fsFlags := pflag.NewFlagSet("floodcal", pflag.ContinueOnError)
	fsFlags.SetOutput(usage)
	fsFlags.SortFlags = false

	configFlag := fsFlags.String("config", "", "config file (default "+DefaultConfigPath()+"; env "+ConfigEnv+")")
	versionFlag := fsFlags.Bool("version", false, "print version and exit")
	for _, o := range options {
		help := fmt.Sprintf("%s (env %s)", o.help, EnvName(o.name))
		switch o.kind {
		case kindString:
			fsFlags.String(o.name, o.def, help)
		case kindInt:
			n, _ := strconv.Atoi(o.def)
			fsFlags.Int(o.name, n, help)
		case kindFloat:
			f, _ := strconv.ParseFloat(o.def, 64)
			fsFlags.Float64(o.name, f, help)
		case kindBool:
			b, _ := strconv.ParseBool(o.def)
			fsFlags.Bool(o.name, b, help)
		}
	}
	fsFlags.Usage = func() {
		fmt.Fprintf(usage, "Usage: floodcal [flags]\n\nFlags (precedence: flag > env > config file > default):\n")
		fsFlags.PrintDefaults()
	}
	if err := fsFlags.Parse(args); err != nil {
		return nil, err
	}
	if fsFlags.NArg() > 0 {
		return nil, fmt.Errorf("unexpected arguments: %v", fsFlags.Args())
	}
	if *versionFlag {
		return &Result{ShowVersion: true}, nil
	}

	c := &Config{}
	// 1. defaults
	for _, o := range options {
		if err := o.set(c, o.def); err != nil {
			return nil, fmt.Errorf("internal: bad default for %s: %w", o.name, err)
		}
	}

	// 2. config file
	path, explicit := DefaultConfigPath(), false
	if fsFlags.Changed("config") {
		path, explicit = *configFlag, true
	} else if v, ok := lookupEnv(ConfigEnv); ok && v != "" {
		path, explicit = v, true
	}
	if path != "" {
		values, err := readYAML(path)
		switch {
		case err == nil:
			c.ConfigPath = path
			for _, o := range options {
				if v, ok := values[o.name]; ok {
					if err := o.set(c, v); err != nil {
						return nil, fmt.Errorf("config %s: %s: %w", path, o.name, err)
					}
				}
			}
		case errors.Is(err, fs.ErrNotExist) && !explicit:
			// default config file is optional
		default:
			return nil, err
		}
	}

	// 3. environment
	for _, o := range options {
		if v, ok := lookupEnv(EnvName(o.name)); ok {
			if err := o.set(c, v); err != nil {
				return nil, fmt.Errorf("env %s: %w", EnvName(o.name), err)
			}
		}
	}

	// 4. flags that were explicitly set
	for _, o := range options {
		if fsFlags.Changed(o.name) {
			if err := o.set(c, fsFlags.Lookup(o.name).Value.String()); err != nil {
				return nil, fmt.Errorf("--%s: %w", o.name, err)
			}
		}
	}

	if c.ServiceAccountKeyfile == "" {
		if v, ok := lookupEnv(GoogleCredentialsEnv); ok {
			c.ServiceAccountKeyfile = v
		}
	}

	if err := c.validate(now); err != nil {
		return nil, err
	}
	return &Result{Config: c}, nil
}

// readYAML reads a flat YAML mapping of option name -> scalar. Unknown keys
// and non-scalar values are errors so typos in a cron config fail loudly.
func readYAML(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("config file %s: %w", path, err)
		}
		return nil, fmt.Errorf("reading config file %s: %w", path, err)
	}
	var root yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&root); err != nil {
		if errors.Is(err, io.EOF) {
			return map[string]string{}, nil // empty file
		}
		return nil, fmt.Errorf("parsing config file %s: %w", path, err)
	}
	if len(root.Content) == 0 {
		return map[string]string{}, nil
	}
	m := root.Content[0]
	if m.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("config file %s: top level must be a mapping of option: value", path)
	}
	known := make(map[string]bool, len(options))
	for _, o := range options {
		known[o.name] = true
	}
	out := make(map[string]string, len(m.Content)/2)
	for i := 0; i+1 < len(m.Content); i += 2 {
		k, v := m.Content[i], m.Content[i+1]
		if !known[k.Value] {
			return nil, fmt.Errorf("config file %s line %d: unknown option %q (valid: %s)", path, k.Line, k.Value, strings.Join(OptionNames(), ", "))
		}
		if v.Kind != yaml.ScalarNode {
			return nil, fmt.Errorf("config file %s line %d: %s must be a single value", path, v.Line, k.Value)
		}
		if v.Tag == "!!null" {
			continue // "key:" with no value means "use the default"
		}
		out[k.Value] = v.Value
	}
	return out, nil
}

func (c *Config) validate(now time.Time) error {
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return fmt.Errorf("timezone %q: %w", c.Timezone, err)
	}
	c.Location = loc
	if c.Start == "" {
		y, m, d := now.In(loc).Date()
		c.StartDate = time.Date(y, m, d, 0, 0, 0, 0, loc)
	} else {
		t, err := time.ParseInLocation("2006-01-02", c.Start, loc)
		if err != nil {
			return fmt.Errorf("start %q: want YYYY-MM-DD", c.Start)
		}
		c.StartDate = t
	}
	if c.Days < 1 || c.Days > 365 {
		return fmt.Errorf("days must be between 1 and 365, got %d", c.Days)
	}
	if c.RoundMinutes < 1 {
		return fmt.Errorf("round-minutes must be >= 1, got %d", c.RoundMinutes)
	}
	if c.MergeGap < 0 {
		return fmt.Errorf("merge-gap must be >= 0, got %d", c.MergeGap)
	}
	switch c.Mode {
	case "list", "json", "ics":
	case "sync":
		if c.CalendarID == "" {
			return fmt.Errorf("mode sync requires calendar-id")
		}
		if c.ServiceAccountKeyfile == "" {
			return fmt.Errorf("mode sync requires service-account-keyfile (or $%s)", GoogleCredentialsEnv)
		}
	default:
		return fmt.Errorf("mode must be list, json, ics or sync, got %q", c.Mode)
	}
	switch c.Source {
	case "hybrid", "forecast", "tide":
	default:
		return fmt.Errorf("source must be hybrid, forecast or tide, got %q", c.Source)
	}
	return nil
}

// EndDate is local midnight after the last day of the window.
func (c *Config) EndDate() time.Time {
	return c.StartDate.AddDate(0, 0, c.Days)
}
