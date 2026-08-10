// Package config loads sabon's runtime configuration: process-level settings
// from SABON_* environment variables, and the backup targets from a YAML file.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/davidborzek/sabon/api"
	"gopkg.in/yaml.v3"
)

// Config is sabon's fully-resolved runtime configuration.
type Config struct {
	LabelPrefix         string
	Runtime             string // SABON_RUNTIME: "auto"(default)|"standalone"|"swarm" — which container runtime to drive
	WatchByDefault      bool
	ConfigFile          string
	ResyncInterval      time.Duration
	DebounceDelay       time.Duration
	MetricsAddr         string
	LogLevel            string
	MoverImage          string            // empty -> auto-detect from sabon's own container
	MoverUser           string            // uid[:gid] the mover runs as; movers need to read arbitrary data
	MoverGroups         []string          // SABON_MOVER_GROUPS: extra supplementary groups (GIDs) added to movers
	MoverNetwork        string            // optional docker network to attach movers to
	MoverLabels         map[string]string // SABON_MOVER_LABELS: extra labels set on every mover container (key=value,...); sabon's own labels always win
	CacheVolume         string            // shared restic cache volume name
	MoverHistory        int               // SABON_MOVER_HISTORY: exited movers kept per app/target/action for run history; 0 = keep none
	RunOnStartup        bool
	Instance            string        // SABON_INSTANCE: only manage containers whose sabon.instance matches; empty = all
	MaxParallel         int           // SABON_MAX_PARALLEL: cap concurrent movers; 0 = unlimited
	ScheduleJitter      time.Duration // SABON_SCHEDULE_JITTER: random 0..jitter delay before scheduled runs
	BackupTimeout       time.Duration // SABON_BACKUP_TIMEOUT: per-backup deadline; 0 = none (labels may override)
	Snapshot            string        // SABON_SNAPSHOT: default snapshot mode "none"|"zfs"|"auto" (labels may override per app)
	SnapshotZFSImage    string        // SABON_SNAPSHOT_ZFS_IMAGE: privileged snapshotter image the zfs provider uses to run the host `zfs` CLI; empty resolves to the version-matched default
	NotifyURLs          []string      // SABON_NOTIFY_URLS: shoutrrr URLs (comma-separated); empty disables
	NotifyOn            string        // "failure" (default) or "always"
	NotifyTitleTemplate string        // SABON_NOTIFY_TITLE_TEMPLATE (inline or @file); empty = built-in default
	NotifyTemplate      string        // SABON_NOTIFY_TEMPLATE (inline or @file); empty = built-in default
	APIAddr             string        // SABON_API_ADDR: control HTTP API listen addr; empty disables
	APIToken            string        // SABON_API_TOKEN: bearer token required when the API is enabled
	Targets             []api.Target
}

// Load reads SABON_* environment and, if present, the targets file.
func Load() (*Config, error) {
	c := &Config{
		Runtime:          env("SABON_RUNTIME", "auto"),
		LabelPrefix:      env("SABON_LABEL_PREFIX", "sabon"),
		WatchByDefault:   envBool("SABON_WATCH_BY_DEFAULT", false),
		ConfigFile:       env("SABON_CONFIG", "/etc/sabon/targets.yaml"),
		ResyncInterval:   envDuration("SABON_RESYNC_INTERVAL", 5*time.Minute),
		DebounceDelay:    envDuration("SABON_DEBOUNCE_DELAY", 2*time.Second),
		MetricsAddr:      env("SABON_METRICS_ADDR", ":9333"),
		LogLevel:         env("SABON_LOG_LEVEL", "info"),
		MoverImage:       env("SABON_MOVER_IMAGE", ""),
		MoverUser:        env("SABON_MOVER_USER", "0:0"),
		MoverGroups:      envList("SABON_MOVER_GROUPS"),
		MoverNetwork:     env("SABON_MOVER_NETWORK", ""),
		CacheVolume:      env("SABON_CACHE_VOLUME", "sabon-cache"),
		MoverHistory:     envInt("SABON_MOVER_HISTORY", 3),
		RunOnStartup:     envBool("SABON_RUN_ON_STARTUP", false),
		Instance:         env("SABON_INSTANCE", ""),
		MaxParallel:      envInt("SABON_MAX_PARALLEL", 0),
		ScheduleJitter:   envDuration("SABON_SCHEDULE_JITTER", 0),
		BackupTimeout:    envDuration("SABON_BACKUP_TIMEOUT", 0),
		Snapshot:         env("SABON_SNAPSHOT", "none"),
		SnapshotZFSImage: env("SABON_SNAPSHOT_ZFS_IMAGE", ""),
		NotifyURLs:       envList("SABON_NOTIFY_URLS"),
		NotifyOn:         env("SABON_NOTIFY_ON", "failure"),
		APIAddr:          env("SABON_API_ADDR", ""),
		APIToken:         env("SABON_API_TOKEN", ""),
	}
	if c.ResyncInterval <= 0 {
		c.ResyncInterval = 5 * time.Minute
	}
	if c.DebounceDelay <= 0 {
		c.DebounceDelay = 2 * time.Second
	}
	if c.Snapshot != "none" && c.Snapshot != "zfs" && c.Snapshot != "auto" {
		return nil, fmt.Errorf("SABON_SNAPSHOT must be \"none\", \"zfs\" or \"auto\", got %q", c.Snapshot)
	}
	if c.Runtime != "auto" && c.Runtime != "standalone" && c.Runtime != "swarm" {
		return nil, fmt.Errorf("SABON_RUNTIME must be \"auto\", \"standalone\" or \"swarm\", got %q", c.Runtime)
	}
	if c.NotifyOn != "failure" && c.NotifyOn != "always" {
		return nil, fmt.Errorf("SABON_NOTIFY_ON must be \"failure\" or \"always\", got %q", c.NotifyOn)
	}
	var err error
	if c.NotifyTitleTemplate, err = envTemplate("SABON_NOTIFY_TITLE_TEMPLATE"); err != nil {
		return nil, err
	}
	if c.NotifyTemplate, err = envTemplate("SABON_NOTIFY_TEMPLATE"); err != nil {
		return nil, err
	}
	if c.MoverLabels, err = envMap("SABON_MOVER_LABELS"); err != nil {
		return nil, err
	}

	f, err := LoadTargets(c.ConfigFile)
	if err != nil {
		return nil, err
	}
	c.Targets = f.Targets
	return c, nil
}

// LoadTargets reads and validates the targets file. A missing file is not an
// error (sabon can run with zero targets and discover nothing to do), but a
// present-but-invalid file is.
func LoadTargets(path string) (*api.File, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &api.File{}, nil
		}
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var f api.File
	if err := dec.Decode(&f); err != nil {
		if errors.Is(err, io.EOF) {
			return &api.File{}, nil // empty file = zero targets
		}
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := f.Validate(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return &f, nil
}

// Target returns the named target, or false.
func (c *Config) Target(name string) (api.Target, bool) {
	for _, t := range c.Targets {
		if t.Name == name {
			return t, true
		}
	}
	return api.Target{}, false
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	if v, ok := os.LookupEnv(key); ok {
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			return b
		}
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return fallback
}
func envDuration(key string, fallback time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok {
		if d, err := time.ParseDuration(strings.TrimSpace(v)); err == nil {
			return d
		}
	}
	return fallback
}

// envList splits a comma-separated env var into trimmed, non-empty entries.
func envList(key string) []string {
	v, ok := os.LookupEnv(key)
	if !ok {
		return nil
	}
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// envMap parses a comma-separated env var of key=value pairs into a map.
// Keys must be non-empty and, since they become container labels alongside
// sabon's own, must not use the reserved "sabon." prefix. Empty/unset -> nil.
func envMap(key string) (map[string]string, error) {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return nil, nil
	}
	out := map[string]string{}
	for _, pair := range strings.Split(v, ",") {
		if strings.TrimSpace(pair) == "" {
			continue
		}
		k, val, found := strings.Cut(pair, "=")
		k = strings.TrimSpace(k)
		if !found || k == "" {
			return nil, fmt.Errorf("%s: %q is not a key=value pair", key, strings.TrimSpace(pair))
		}
		if strings.HasPrefix(k, "sabon.") {
			return nil, fmt.Errorf("%s: key %q uses the reserved \"sabon.\" prefix", key, k)
		}
		out[k] = strings.TrimSpace(val)
	}
	return out, nil
}

// envTemplate reads a template from an env var. A value beginning with "@" is
// read from the named file (so large multi-line templates need not live in the
// environment); otherwise the value is used verbatim. Empty/unset yields "".
func envTemplate(key string) (string, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return "", nil
	}
	if path, cut := strings.CutPrefix(v, "@"); cut {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("%s: read template file: %w", key, err)
		}
		return string(b), nil
	}
	return v, nil
}
