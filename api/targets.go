package api

import (
	"fmt"
	"path"
	"strings"
)

// Backend is how a target's restic repository is located.
type Backend string

const (
	// BackendLocal is a local filesystem repo: Path is a host base directory,
	// the per-app repo is <Path>/<repo>, bind-mounted into the mover.
	BackendLocal Backend = "local"
	// BackendRemote is any restic repo string (s3:, rest:, sftp:, b2:, …) with
	// {app} and ${ENV} placeholders; nothing is mounted.
	BackendRemote Backend = "remote"
)

// Target is one backup destination shared by every app. The per-app repository
// is derived from the app's repo name.
type Target struct {
	// Name identifies the target (used in labels, metrics, schedule keys).
	Name string `yaml:"name" json:"name" jsonschema:"description=Target name,required"`
	// Path is the host base directory for a local backend. The per-app repo is
	// <Path>/<repo>. Mutually exclusive with Repo.
	Path string `yaml:"path,omitempty" json:"path,omitempty" jsonschema:"description=Host base directory for a local repo backend (repo = <Path>/<app>)"`
	// Repo is a restic repository string for a remote backend, with {app} and
	// ${ENV} placeholders. Mutually exclusive with Path.
	Repo string `yaml:"repo,omitempty" json:"repo,omitempty" jsonschema:"description=restic repository string for a remote backend; supports {app} and ${ENV} placeholders"`
	// PasswordEnv names the environment variable (in sabon's own environment)
	// holding the repository password. Defaults to RESTIC_PASSWORD.
	PasswordEnv string `yaml:"passwordEnv,omitempty" json:"passwordEnv,omitempty" jsonschema:"description=Env var holding the repo password (default: RESTIC_PASSWORD)"`
	// PasswordFile is a host path to a file containing the repository password.
	// When set it is mounted read-only into the mover and restic reads it via
	// RESTIC_PASSWORD_FILE, so the password never appears in any container's
	// environment. Takes precedence over PasswordEnv.
	PasswordFile string `yaml:"passwordFile,omitempty" json:"passwordFile,omitempty" jsonschema:"description=Host path to a file holding the repo password; mounted into the mover and read via RESTIC_PASSWORD_FILE (keeps it out of env). Overrides passwordEnv"`
	// CredentialsFile is a host path to an AWS shared-credentials INI file
	// ([default] with aws_access_key_id / aws_secret_access_key). It is mounted
	// read-only into the mover and exposed via AWS_SHARED_CREDENTIALS_FILE, so
	// S3/B2/… backend credentials stay out of the mover's environment. Do not
	// also set AWS_* in env (env takes precedence over the file).
	CredentialsFile string `yaml:"credentialsFile,omitempty" json:"credentialsFile,omitempty" jsonschema:"description=Host path to an AWS shared-credentials file; mounted into the mover and used via AWS_SHARED_CREDENTIALS_FILE (keeps S3 creds out of env)"`
	// Files are host files mounted read-only into every mover of this target
	// (SSH keys, known_hosts, rclone config, …). sabon sets no env for them;
	// reference Target from env (e.g. RCLONE_CONFIG_X_KEY_FILE).
	Files []TargetFile `yaml:"files,omitempty" json:"files,omitempty" jsonschema:"description=Host files mounted read-only into the mover (SSH key or known_hosts or rclone config); reference the target path from env"`
	// Env are extra environment variables passed to the mover (e.g. S3/R2
	// credentials). Values support ${ENV} expansion from sabon's environment.
	Env map[string]string `yaml:"env,omitempty" json:"env,omitempty" jsonschema:"description=Extra environment for the mover (e.g. AWS_ACCESS_KEY_ID); values support ${ENV} expansion"`
	// ResticArgs are extra global restic flags (e.g. --limit-upload, --pack-size,
	// --compression, -o key=value) prepended to every restic invocation for this
	// target.
	ResticArgs []string `yaml:"resticArgs,omitempty" json:"resticArgs,omitempty" jsonschema:"description=Extra global restic flags for this target (e.g. --limit-upload, --pack-size, -o key=value); prepended to every invocation"`
	// MoverLabels are extra labels set on every mover container spawned for this
	// target, merged on top of the global SABON_MOVER_LABELS (target wins).
	// Keys must not use the reserved "sabon." prefix.
	MoverLabels map[string]string `yaml:"moverLabels,omitempty" json:"moverLabels,omitempty" jsonschema:"description=Extra labels set on every mover container for this target (merged over SABON_MOVER_LABELS; target wins). Keys must not use the reserved sabon. prefix"`
	// Schedule is the default cron for this target (6-field, seconds first).
	Schedule string `yaml:"schedule,omitempty" json:"schedule,omitempty" jsonschema:"description=Default cron schedule (6-field, seconds first)"`
	// Retention is the default forget policy for this target.
	Retention Retention `yaml:"retention,omitempty" json:"retention,omitempty" jsonschema:"description=Default retention policy"`
	// Check is an optional cron for `restic check` (repository integrity
	// verification) against this target's per-app repos. Empty disables it.
	Check string `yaml:"check,omitempty" json:"check,omitempty" jsonschema:"description=Optional cron for periodic restic check (repo integrity); empty disables"`
	// Prune is an optional cron for `restic prune` (repack/reclaim space) of
	// this target's per-app repos. Empty disables it; backups then only run
	// `forget` (mark) and space is reclaimed by this scheduled prune.
	Prune string `yaml:"prune,omitempty" json:"prune,omitempty" jsonschema:"description=Optional cron for periodic restic prune (reclaim space); empty disables"`
}

// TargetFile is one host file mounted read-only into a target's movers.
type TargetFile struct {
	// Source is the host path of the file.
	Source string `yaml:"source" json:"source" jsonschema:"description=Host path of the file,required"`
	// Target is the absolute path inside the mover.
	Target string `yaml:"target" json:"target" jsonschema:"description=Absolute path inside the mover,required"`
}

// reservedMoverPaths are mover paths sabon mounts itself; keep in sync with the
// mount constants in internal/backup.
var reservedMoverPaths = []string{
	"/data",
	"/repo",
	"/cache",
	"/restore",
	"/run/secrets/sabon-restic-password",
	"/run/secrets/sabon-aws-credentials",
}

// ReservedMoverPaths returns a copy of the mover paths sabon mounts itself.
func ReservedMoverPaths() []string {
	return append([]string(nil), reservedMoverPaths...)
}

func (t Target) validateFiles() error {
	seen := map[string]bool{}
	for i, f := range t.Files {
		if f.Source == "" || f.Target == "" {
			return fmt.Errorf("target %q: files[%d]: source and target are required", t.Name, i)
		}
		if !isCleanHostFile(f.Source) {
			return fmt.Errorf("target %q: files[%d]: source %q must be a clean absolute path without ':'", t.Name, i, f.Source)
		}
		if strings.Contains(f.Target, ":") {
			return fmt.Errorf("target %q: files[%d]: target %q must not contain ':'", t.Name, i, f.Target)
		}
		if !path.IsAbs(f.Target) {
			return fmt.Errorf("target %q: files[%d]: target %q must be an absolute path", t.Name, i, f.Target)
		}
		dst := path.Clean(f.Target)
		if dst == "/" {
			return fmt.Errorf("target %q: files[%d]: target must not be /", t.Name, i)
		}
		for _, r := range reservedMoverPaths {
			// A parent of a reserved path would shadow sabon's own mounts.
			if dst == r || strings.HasPrefix(dst, r+"/") || strings.HasPrefix(r, dst+"/") {
				return fmt.Errorf("target %q: files[%d]: target %q collides with reserved mover path %q", t.Name, i, f.Target, r)
			}
		}
		if seen[dst] {
			return fmt.Errorf("target %q: files[%d]: duplicate target %q", t.Name, i, f.Target)
		}
		seen[dst] = true
	}
	return nil
}

// Backend classifies the target.
func (t Target) Backend() Backend {
	if t.Path != "" {
		return BackendLocal
	}
	return BackendRemote
}

// File is the on-disk targets configuration.
type File struct {
	Targets []Target `yaml:"targets" json:"targets" jsonschema:"description=Backup targets,required"`
}

// Validate reports whether the targets file is well-formed.
func (f *File) Validate() error {
	seen := map[string]bool{}
	for i, t := range f.Targets {
		if t.Name == "" {
			return fmt.Errorf("targets[%d]: name is required", i)
		}
		if seen[t.Name] {
			return fmt.Errorf("target %q: duplicate name", t.Name)
		}
		seen[t.Name] = true
		if (t.Path == "") == (t.Repo == "") {
			return fmt.Errorf("target %q: set exactly one of path or repo", t.Name)
		}
		for k := range t.MoverLabels {
			if strings.HasPrefix(k, "sabon.") {
				return fmt.Errorf("target %q: moverLabels key %q uses the reserved \"sabon.\" prefix", t.Name, k)
			}
		}
		for field, p := range map[string]string{"passwordFile": t.PasswordFile, "credentialsFile": t.CredentialsFile} {
			if p != "" && !isCleanHostFile(p) {
				return fmt.Errorf("target %q: %s %q must be a clean absolute path without ':'", t.Name, field, p)
			}
		}
		if err := t.validateFiles(); err != nil {
			return err
		}
	}
	return nil
}

// isCleanHostFile reports whether p can be bind-mounted as-is: Docker
// path.Cleans bind sources, so an unclean path would be checked and mounted as
// different files once a symlink precedes "..".
func isCleanHostFile(p string) bool {
	return path.IsAbs(p) && path.Clean(p) == p && !strings.Contains(p, ":")
}
