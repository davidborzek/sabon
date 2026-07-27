// Package api is sabon's configuration API: the container
// backup label schema (Spec, TargetSpec, Retention, Hook) and the on-disk
// targets file schema (File, Target, Backend). These types are the stable,
// machine-schema'd contract emitted by `sabon schema`; extraction from Docker
// labels and loading from disk live in the internal packages.
package api

import (
	"bytes"
	"fmt"
	"strconv"
	"time"

	"github.com/invopop/jsonschema"
	"gopkg.in/yaml.v3"
)

// Retention maps restic's forget policy. Zero fields are omitted; an empty
// Retention means "keep everything" (no forget runs).
type Retention struct {
	Last    int    `yaml:"last,omitempty" json:"last,omitempty" jsonschema:"description=Keep the N most recent snapshots"`
	Hourly  int    `yaml:"hourly,omitempty" json:"hourly,omitempty" jsonschema:"description=Keep N hourly snapshots"`
	Daily   int    `yaml:"daily,omitempty" json:"daily,omitempty" jsonschema:"description=Keep N daily snapshots"`
	Weekly  int    `yaml:"weekly,omitempty" json:"weekly,omitempty" jsonschema:"description=Keep N weekly snapshots"`
	Monthly int    `yaml:"monthly,omitempty" json:"monthly,omitempty" jsonschema:"description=Keep N monthly snapshots"`
	Yearly  int    `yaml:"yearly,omitempty" json:"yearly,omitempty" jsonschema:"description=Keep N yearly snapshots"`
	Within  string `yaml:"within,omitempty" json:"within,omitempty" jsonschema:"description=Keep all snapshots within a duration (restic --keep-within, e.g. 30d)"`
}

// Empty reports whether no retention rule is set.
func (r Retention) Empty() bool {
	return r.Last == 0 && r.Hourly == 0 && r.Daily == 0 && r.Weekly == 0 &&
		r.Monthly == 0 && r.Yearly == 0 && r.Within == ""
}

// ForgetArgs renders the retention as restic `forget` flags. Returns nil when
// empty (caller must then skip forget to avoid deleting everything).
func (r Retention) ForgetArgs() []string {
	var a []string
	add := func(flag string, n int) {
		if n > 0 {
			a = append(a, flag, strconv.Itoa(n))
		}
	}
	add("--keep-last", r.Last)
	add("--keep-hourly", r.Hourly)
	add("--keep-daily", r.Daily)
	add("--keep-weekly", r.Weekly)
	add("--keep-monthly", r.Monthly)
	add("--keep-yearly", r.Yearly)
	if r.Within != "" {
		a = append(a, "--keep-within", r.Within)
	}
	return a
}

// Hook runs a command around a backup, in one of two modes:
//   - exec (default): exec Command inside an existing container (Container,
//     defaulting to the labeled container);
//   - run: spawn a fresh one-shot container from Image and run Command in it
//     (e.g. a throwaway postgres client that dumps a DB over the network into a
//     backed-up volume). Set Image to select this mode.
type Hook struct {
	// Command is the argv to execute. Required.
	Command []string `yaml:"command" json:"command" jsonschema:"description=Command argv to execute,required"`
	// Container (exec mode) is the existing container to exec in; defaults to
	// the labeled container. Mutually exclusive with Image.
	Container string `yaml:"container,omitempty" json:"container,omitempty" jsonschema:"description=exec mode: existing container to exec in (default: the labeled container)"`
	// Image (run mode) spawns a fresh one-shot container from this image and
	// runs Command in it, instead of exec-ing an existing container.
	Image string `yaml:"image,omitempty" json:"image,omitempty" jsonschema:"description=run mode: image for a fresh one-shot container (instead of exec-ing an existing one)"`
	// User overrides the user (exec user, or the one-shot container's user).
	User string `yaml:"user,omitempty" json:"user,omitempty" jsonschema:"description=User to run the command as"`
	// Env are extra environment variables; values support ${ENV} expansion
	// from sabon's own environment (e.g. to inject a DB password).
	Env map[string]string `yaml:"env,omitempty" json:"env,omitempty" jsonschema:"description=Extra environment variables; values support ${ENV} expansion"`
	// Network (run mode) attaches the one-shot container to a Docker network so
	// it can reach other services by name.
	Network string `yaml:"network,omitempty" json:"network,omitempty" jsonschema:"description=run mode: Docker network to attach the one-shot container to"`
	// Volumes (run mode) are mounts as "source:/target[:ro]"; source is a
	// volume name or an absolute host path.
	Volumes []string `yaml:"volumes,omitempty" json:"volumes,omitempty" jsonschema:"description=run mode: mounts as source:/target[:ro] (source = volume name or host path)"`
	// Timeout bounds the hook (Go duration, e.g. "5m"); empty = no timeout.
	Timeout string `yaml:"timeout,omitempty" json:"timeout,omitempty" jsonschema:"description=Optional hook timeout as a Go duration (e.g. 5m); empty = no timeout"`
}

// Mode reports whether the hook spawns a one-shot container ("run") or execs an
// existing one ("exec").
func (h Hook) Mode() string {
	if h.Image != "" {
		return "run"
	}
	return "exec"
}

// TimeoutDuration parses Timeout; a zero duration means "no timeout".
func (h Hook) TimeoutDuration() (time.Duration, error) {
	if h.Timeout == "" {
		return 0, nil
	}
	return time.ParseDuration(h.Timeout)
}

// Spec is the per-container backup specification, carried in the document-form
// label <prefix>.backup as embedded YAML.
type Spec struct {
	// Repo is the per-app repository name (the {app} in a target's template).
	// Defaults to the Compose service name, else the container name.
	Repo string `yaml:"repo,omitempty" json:"repo,omitempty" jsonschema:"description=Repository name (the {app} in a target template); default: compose service/container name"`
	// Auto includes the labeled container's own bind mounts and named volumes
	// as backup sources. Defaults to true.
	Auto *bool `yaml:"auto,omitempty" json:"auto,omitempty" jsonschema:"description=Include the labeled container's own mounts as sources (default: true)"`
	// ExtraVolumes are extra named volumes to back up (may belong to other
	// containers of the same app, referenced by volume name).
	ExtraVolumes []string `yaml:"extraVolumes,omitempty" json:"extraVolumes,omitempty" jsonschema:"description=Extra named volumes to include on top of the auto-discovered set"`
	// ExcludeVolumes drops named volumes from the auto-discovered set (auto:
	// true) — e.g. a cache/scratch/live-DB volume — while still auto-including
	// the container's other mounts. Exact volume names; does not affect volumes
	// added explicitly via ExtraVolumes. Distinct from Exclude (restic file patterns).
	ExcludeVolumes []string `yaml:"excludeVolumes,omitempty" json:"excludeVolumes,omitempty" jsonschema:"description=Named volumes to drop from the auto-discovered set (auto: true); does not affect the explicit extraVolumes list or file-level exclude patterns"`
	// ExtraPaths are extra host bind paths to back up.
	ExtraPaths []string `yaml:"extraPaths,omitempty" json:"extraPaths,omitempty" jsonschema:"description=Extra host paths (bind mounts) to include"`
	// Exclude are restic exclude patterns applied to the backup.
	Exclude []string `yaml:"exclude,omitempty" json:"exclude,omitempty" jsonschema:"description=restic --exclude patterns"`
	// Tags are added to every snapshot (the repo name is always tagged).
	Tags []string `yaml:"tags,omitempty" json:"tags,omitempty" jsonschema:"description=Extra restic tags added to each snapshot"`
	// Targets selects which configured targets to back up to, each optionally
	// with per-target overrides. Empty = all configured targets with defaults.
	// In the label an entry is a bare target name or a mapping with a name plus
	// overrides.
	Targets []TargetSpec `yaml:"targets,omitempty" json:"targets,omitempty" jsonschema:"description=Targets to back up to; each entry is a target name or an object with a name plus per-target overrides. Default: all configured targets"`
	// Stop stops the labeled container for the duration of the backup (cold
	// backup) and restarts it afterwards.
	Stop bool `yaml:"stop,omitempty" json:"stop,omitempty" jsonschema:"description=Stop the labeled container during the backup and restart it afterwards"`
	// PreHooks run before the backup (e.g. pg_dump into a backed-up volume).
	PreHooks []Hook `yaml:"preHooks,omitempty" json:"preHooks,omitempty" jsonschema:"description=Commands to exec before the backup"`
	// PostHooks run after the backup, regardless of outcome.
	PostHooks []Hook `yaml:"postHooks,omitempty" json:"postHooks,omitempty" jsonschema:"description=Commands to exec after the backup"`
	// Timeout overrides the global backup deadline for this app (Go duration,
	// e.g. "2h"); empty uses SABON_BACKUP_TIMEOUT.
	Timeout string `yaml:"timeout,omitempty" json:"timeout,omitempty" jsonschema:"description=Per-app backup timeout as a Go duration (e.g. 2h); empty uses the global default"`
	// User overrides SABON_MOVER_USER for this app's movers ("uid[:gid]"), to
	// match the ownership of the app's data volumes. Empty uses the global
	// default. Applies to backup and in-place restore (the paths that touch the
	// app's volumes); repo-only actions (check/prune/snapshots) use the default.
	User string `yaml:"user,omitempty" json:"user,omitempty" jsonschema:"description=uid[:gid] the mover runs as for this app; overrides SABON_MOVER_USER"`
	// Groups are extra supplementary groups (GIDs) added to this app's movers,
	// so a non-root mover can read group-owned source files. Empty uses the
	// global SABON_MOVER_GROUPS. Applies to backup and in-place restore.
	Groups []string `yaml:"groups,omitempty" json:"groups,omitempty" jsonschema:"description=Extra supplementary groups (GIDs) added to the mover; overrides SABON_MOVER_GROUPS"`
	// Snapshot selects the source-snapshot strategy for this app
	// ("none"|"zfs"|"auto"). Empty uses the global SABON_SNAPSHOT.
	Snapshot string `yaml:"snapshot,omitempty" json:"snapshot,omitempty" jsonschema:"description=Source snapshot strategy: none | zfs | auto (auto snapshots ZFS sources and mounts the rest live); overrides SABON_SNAPSHOT"`
}

// TargetSpec selects a target to back up to and optionally overrides its
// schedule, retention and source set for that target. In the label it is
// written either as a bare target name (string) or as a mapping with a `name`
// plus overrides.
type TargetSpec struct {
	Name           string    `yaml:"name" json:"name" jsonschema:"description=Target name"`
	Schedule       string    `yaml:"schedule,omitempty" json:"schedule,omitempty" jsonschema:"description=Per-target cron override (6-field; seconds first)"`
	Retention      Retention `yaml:"retention,omitempty" json:"retention,omitempty" jsonschema:"description=Per-target retention override"`
	ExcludeVolumes []string  `yaml:"excludeVolumes,omitempty" json:"excludeVolumes,omitempty" jsonschema:"description=Named volumes not backed up to this target"`
	ExcludePaths   []string  `yaml:"excludePaths,omitempty" json:"excludePaths,omitempty" jsonschema:"description=Host bind paths not backed up to this target"`
	Exclude        []string  `yaml:"exclude,omitempty" json:"exclude,omitempty" jsonschema:"description=Extra restic --exclude patterns for this target"`
}

// JSONSchemaExtend lets a target be written as a bare name (string) as well as
// the object form with per-target overrides.
func (TargetSpec) JSONSchemaExtend(s *jsonschema.Schema) {
	obj := *s
	*s = jsonschema.Schema{
		OneOf: []*jsonschema.Schema{
			{Type: "string", Description: "Target name"},
			&obj,
		},
	}
}

// UnmarshalYAML accepts either a bare target name (scalar) or a mapping with a
// name plus overrides.
func (t *TargetSpec) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		if err := n.Decode(&t.Name); err != nil {
			return err
		}
		if t.Name == "" {
			return fmt.Errorf("target: name is required")
		}
		return nil
	}
	type raw TargetSpec // avoid recursing into this method
	// n.Decode does not honour the parent decoder's KnownFields, so re-decode
	// strictly: typos fail closed at every level (top-level and nested, e.g. a
	// misspelled retention key), like the rest of the spec.
	data, err := yaml.Marshal(n)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode((*raw)(t)); err != nil {
		return err
	}
	if t.Name == "" {
		return fmt.Errorf("target: name is required")
	}
	return nil
}

// TargetOverride returns the per-target override for name, if the spec lists it.
func (s Spec) TargetOverride(name string) (TargetSpec, bool) {
	for _, t := range s.Targets {
		if t.Name == name {
			return t, true
		}
	}
	return TargetSpec{}, false
}

// TargetNames returns the explicitly selected target names (empty = all).
func (s Spec) TargetNames() []string {
	names := make([]string, len(s.Targets))
	for i, t := range s.Targets {
		names[i] = t.Name
	}
	return names
}

// BackupTimeout parses the per-app Timeout; zero means "use the global default".
func (s Spec) BackupTimeout() (time.Duration, error) {
	if s.Timeout == "" {
		return 0, nil
	}
	return time.ParseDuration(s.Timeout)
}

// MoverUser returns the per-app mover user, falling back to the global default
// when the label does not set one.
func (s Spec) MoverUser(fallback string) string {
	if s.User == "" {
		return fallback
	}
	return s.User
}

// MoverGroups returns the per-app supplementary groups, falling back to the
// global default when the label does not set any.
func (s Spec) MoverGroups(fallback []string) []string {
	if len(s.Groups) == 0 {
		return fallback
	}
	return s.Groups
}

// SnapshotMode returns the per-app snapshot strategy, falling back to the
// global default when the label does not set one.
func (s Spec) SnapshotMode(fallback string) string {
	if s.Snapshot == "" {
		return fallback
	}
	return s.Snapshot
}

// AutoSources reports whether the container's own mounts should be included
// (defaults to true when unset).
func (s Spec) AutoSources() bool { return s.Auto == nil || *s.Auto }
