package config

import (
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

var fixedNow = time.Date(2026, 11, 21, 20, 0, 0, 0, time.UTC) // 12:00 PST

func envMap(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func load(t *testing.T, args []string, env map[string]string) *Config {
	t.Helper()
	// Point the default config path at a nonexistent file so the developer's
	// real ~/.config/floodcal/config.yaml never leaks into tests.
	t.Setenv("HOME", t.TempDir())
	r, err := Load(args, envMap(env), io.Discard, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	return r.Config
}

func TestDefaults(t *testing.T) {
	c := load(t, nil, nil)
	if c.Station != "9414290" || c.Days != 14 || c.MinHeight != 6.0 || c.Mode != "list" ||
		c.Source != "hybrid" || !c.AnomalyOffset || c.TitleTemplate != DefaultTitleTemplate || c.ConfigPath != "" {
		t.Fatalf("%+v", c)
	}
	if want := "2026-11-21T00:00:00-08:00"; c.StartDate.Format(time.RFC3339) != want {
		t.Errorf("start %s", c.StartDate.Format(time.RFC3339))
	}
}

func TestPrecedenceFlagEnvYAMLDefault(t *testing.T) {
	p := writeFile(t, "min-height: 6.5\ndays: 10\nstation: \"111\"\n")
	env := map[string]string{"FLOODCAL_MIN_HEIGHT": "7.0", "FLOODCAL_DAYS": "9"}
	c := load(t, []string{"--config", p, "--min-height", "7.5"}, env)
	if c.MinHeight != 7.5 {
		t.Errorf("flag should win: %v", c.MinHeight)
	}
	if c.Days != 9 {
		t.Errorf("env should beat yaml: %v", c.Days)
	}
	if c.Station != "111" {
		t.Errorf("yaml should beat default: %v", c.Station)
	}
	if c.RoundMinutes != 15 {
		t.Errorf("default: %v", c.RoundMinutes)
	}
}

func TestConfigPathFromEnv(t *testing.T) {
	p := writeFile(t, "days: 3\n")
	c := load(t, nil, map[string]string{ConfigEnv: p})
	if c.Days != 3 || c.ConfigPath != p {
		t.Fatalf("%+v", c)
	}
}

func TestDefaultConfigPathIsUsedWhenPresent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "floodcal")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("days: 4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := Load(nil, envMap(nil), io.Discard, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if r.Config.Days != 4 {
		t.Fatalf("days %d", r.Config.Days)
	}
}

func TestExplicitMissingConfigIsError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := Load([]string{"--config", "/nonexistent/x.yaml"}, envMap(nil), io.Discard, fixedNow); err == nil {
		t.Fatal("expected error")
	}
}

func TestUnknownYAMLKeyIsError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p := writeFile(t, "min_height: 6.5\n")
	_, err := Load([]string{"--config", p}, envMap(nil), io.Discard, fixedNow)
	if err == nil || !strings.Contains(err.Error(), "unknown option") {
		t.Fatalf("err %v", err)
	}
}

func TestBoolFlagsAndEnv(t *testing.T) {
	c := load(t, []string{"--anomaly-offset=false", "--dry-run"}, nil)
	if c.AnomalyOffset || !c.DryRun {
		t.Fatalf("%+v", c)
	}
	c = load(t, nil, map[string]string{"FLOODCAL_ANOMALY_OFFSET": "false"})
	if c.AnomalyOffset {
		t.Fatal("env bool")
	}
}

func TestSyncRequiresCalendarAndKey(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := Load([]string{"--mode", "sync"}, envMap(nil), io.Discard, fixedNow); err == nil {
		t.Fatal("expected calendar-id error")
	}
	if _, err := Load([]string{"--mode", "sync", "--calendar-id", "x"}, envMap(nil), io.Discard, fixedNow); err == nil {
		t.Fatal("expected keyfile error")
	}
	c := load(t, []string{"--mode", "sync", "--calendar-id", "x"}, map[string]string{GoogleCredentialsEnv: "/k.json"})
	if c.ServiceAccountKeyfile != "/k.json" {
		t.Fatalf("fallback: %q", c.ServiceAccountKeyfile)
	}
	c = load(t, []string{"--mode", "sync", "--calendar-id", "x", "--service-account-keyfile", "/mine.json"},
		map[string]string{GoogleCredentialsEnv: "/k.json"})
	if c.ServiceAccountKeyfile != "/mine.json" {
		t.Fatalf("explicit keyfile should win: %q", c.ServiceAccountKeyfile)
	}
}

func TestValidation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, args := range [][]string{
		{"--mode", "bogus"},
		{"--source", "bogus"},
		{"--days", "0"},
		{"--start", "11/21/2026"},
		{"--timezone", "Not/AZone"},
		{"--round-minutes", "0"},
	} {
		if _, err := Load(args, envMap(nil), io.Discard, fixedNow); err == nil {
			t.Errorf("%v: expected error", args)
		}
	}
}

func TestVersionFlag(t *testing.T) {
	r, err := Load([]string{"--version"}, envMap(nil), io.Discard, fixedNow)
	if err != nil || !r.ShowVersion {
		t.Fatalf("%+v %v", r, err)
	}
}

// Every option must be settable from flag, env and YAML.
func TestEveryOptionSettableFromAllSources(t *testing.T) {
	sample := map[kind][2]string{ // [value for yaml/env, value for flag]
		kindString: {"from-src", "from-flag"},
		kindInt:    {"7", "8"},
		kindFloat:  {"7.25", "8.25"},
		kindBool:   {"true", "false"},
	}
	for _, o := range options {
		o := o
		t.Run(o.name, func(t *testing.T) {
			if o.name == "timezone" || o.name == "mode" || o.name == "source" || o.name == "start" {
				t.Skip("validated enum/format; covered by other tests")
			}
			read := func(c *Config) string { return sprintField(c, o.name) }
			v := sample[o.kind]
			p := writeFile(t, o.name+": \""+v[0]+"\"\n")
			c := load(t, []string{"--config", p}, nil)
			if got := read(c); got != v[0] {
				t.Errorf("yaml: got %q want %q", got, v[0])
			}
			c = load(t, nil, map[string]string{EnvName(o.name): v[0]})
			if got := read(c); got != v[0] {
				t.Errorf("env: got %q want %q", got, v[0])
			}
			c = load(t, []string{"--" + o.name + "=" + v[1]}, nil)
			if got := read(c); got != v[1] {
				t.Errorf("flag: got %q want %q", got, v[1])
			}
		})
	}
}

func sprintField(c *Config, name string) string {
	switch name {
	case "station":
		return c.Station
	case "days":
		return itoa(c.Days)
	case "min-height":
		return ftoa(c.MinHeight)
	case "merge-gap":
		return itoa(c.MergeGap)
	case "round-minutes":
		return itoa(c.RoundMinutes)
	case "datum":
		return c.Datum
	case "location-label":
		return c.LocationLabel
	case "title-template":
		return c.TitleTemplate
	case "output":
		return c.Output
	case "anomaly-offset":
		return btoa(c.AnomalyOffset)
	case "calendar-id":
		return c.CalendarID
	case "service-account-keyfile":
		return c.ServiceAccountKeyfile
	case "dry-run":
		return btoa(c.DryRun)
	}
	panic("sprintField: add a case for option " + name)
}

func itoa(n int) string     { return strconv.Itoa(n) }
func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
func btoa(b bool) string    { return strconv.FormatBool(b) }

func TestExampleConfigMatchesOptions(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "configs", "example-config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	var got []string
	for k := range m {
		got = append(got, k)
	}
	want := OptionNames()
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("example keys %v\noptions      %v", got, want)
	}
	// And it must load cleanly.
	t.Setenv("HOME", t.TempDir())
	p := filepath.Join("..", "..", "configs", "example-config.yaml")
	r, err := Load([]string{"--config", p}, envMap(nil), io.Discard, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if r.Config.TitleTemplate != DefaultTitleTemplate {
		t.Errorf("example title-template %q differs from default", r.Config.TitleTemplate)
	}
}
