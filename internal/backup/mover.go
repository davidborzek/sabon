package backup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/davidborzek/sabon/api"
	"github.com/davidborzek/sabon/internal/discovery"
	"github.com/davidborzek/sabon/internal/mover"
	"github.com/docker/docker/api/types/mount"
)

// moverSpec builds the JSON contract for the mover.
func (o *Orchestrator) moverSpec(job discovery.Job, target api.Target) mover.Spec {
	sources := make([]string, 0, len(job.Sources))
	for _, s := range job.Sources {
		sources = append(sources, dataMount+"/"+s.Name)
	}
	ret := target.Retention
	excludes := job.Spec.Exclude
	if ts, ok := job.Spec.TargetOverride(target.Name); ok {
		if !ts.Retention.Empty() {
			ret = ts.Retention
		}
		if len(ts.Exclude) > 0 {
			excludes = append(append([]string{}, excludes...), ts.Exclude...)
		}
	}
	return mover.Spec{
		App:       job.App,
		Host:      job.App,
		Sources:   sources,
		Excludes:  excludes,
		Tags:      job.Spec.Tags,
		Retention: ret,
		ExtraArgs: target.ResticArgs,
	}
}

// sourcesForTarget drops sources a target excludes (per-target excludeVolumes /
// excludePaths, matched by volume name / bind path). Returns job.Sources
// unchanged when the target sets no per-target exclusions.
func (o *Orchestrator) sourcesForTarget(job discovery.Job, target api.Target) []discovery.Source {
	ts, ok := job.Spec.TargetOverride(target.Name)
	if !ok || (len(ts.ExcludeVolumes) == 0 && len(ts.ExcludePaths) == 0) {
		return job.Sources
	}
	exVol := toSet(ts.ExcludeVolumes)
	exPath := toSet(ts.ExcludePaths)
	out := make([]discovery.Source, 0, len(job.Sources))
	for _, s := range job.Sources {
		if s.Type == mount.TypeVolume && exVol[s.Ref] {
			continue
		}
		if s.Type == mount.TypeBind && exPath[s.Ref] {
			continue
		}
		out = append(out, s)
	}
	return out
}

func toSet(xs []string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// sourceMounts maps a job's sources to typed mounts at /data/<name>.
func (o *Orchestrator) sourceMounts(job discovery.Job, readOnly bool) []mount.Mount {
	ms := make([]mount.Mount, 0, len(job.Sources))
	for _, s := range job.Sources {
		ms = append(ms, mount.Mount{
			Type:     s.Type,
			Source:   s.Ref,
			Target:   dataMount + "/" + s.Name,
			ReadOnly: readOnly,
		})
	}
	return ms
}

// cacheAndRepo returns the shared cache mount and, for a local backend, the
// per-app repository bind.
func (o *Orchestrator) cacheAndRepo(target api.Target, app string) ([]mount.Mount, []string) {
	ms := []mount.Mount{{Type: mount.TypeVolume, Source: o.cfg.CacheVolume, Target: cacheMount}}
	var binds []string
	if target.Backend() == api.BackendLocal {
		binds = append(binds, filepath.Join(target.Path, app)+":"+repoMount)
	}
	if target.PasswordFile != "" {
		binds = append(binds, target.PasswordFile+":"+passwordMount+":ro")
	}
	if target.CredentialsFile != "" {
		binds = append(binds, target.CredentialsFile+":"+awsCredentialsMount+":ro")
	}
	return ms, binds
}

// resticEnv builds the environment restic reads inside the mover.
func (o *Orchestrator) resticEnv(target api.Target, app string) ([]string, error) {
	env := []string{"RESTIC_CACHE_DIR=" + cacheMount}

	switch target.Backend() {
	case api.BackendLocal:
		env = append(env, "RESTIC_REPOSITORY="+repoMount)
	case api.BackendRemote:
		repo := strings.ReplaceAll(os.Expand(target.Repo, os.Getenv), "{app}", app)
		env = append(env, "RESTIC_REPOSITORY="+repo)
	}

	if target.PasswordFile != "" {
		fi, err := os.Stat(target.PasswordFile)
		if err != nil || !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("target %q: password file %q does not exist or is not a regular file", target.Name, target.PasswordFile)
		}
		env = append(env, "RESTIC_PASSWORD_FILE="+passwordMount)
	} else {
		pwEnv := target.PasswordEnv
		if pwEnv == "" {
			pwEnv = "RESTIC_PASSWORD"
		}
		pw := os.Getenv(pwEnv)
		if pw == "" {
			return nil, fmt.Errorf("target %q: password env %q is empty", target.Name, pwEnv)
		}
		env = append(env, "RESTIC_PASSWORD="+pw)
	}
	if target.CredentialsFile != "" {
		fi, err := os.Stat(target.CredentialsFile)
		if err != nil || !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("target %q: credentials file %q does not exist or is not a regular file", target.Name, target.CredentialsFile)
		}
		env = append(env, "AWS_SHARED_CREDENTIALS_FILE="+awsCredentialsMount)
	}

	for k, v := range target.Env {
		env = append(env, k+"="+os.Expand(v, os.Getenv))
	}
	return env, nil
}

func (o *Orchestrator) ensureCache(ctx context.Context) error {
	o.cacheMu.Lock()
	defer o.cacheMu.Unlock()
	if o.cacheOK {
		return nil
	}
	if err := o.host.EnsureCache(ctx, o.cfg.CacheVolume); err != nil {
		return err
	}
	o.cacheOK = true
	return nil
}

func moverName(app, target string) string {
	return fmt.Sprintf("sabon-mover-%s-%s-%d", sanitize(app), sanitize(target), time.Now().UnixNano())
}

// hookEnvPrefix is the allowlist prefix for hook ${NAME} references that are not
// satisfied by the labelled container's own environment: only variables named
// SABON_HOOK_ENV_<NAME> in sabon's environment are reachable, never sabon's own
// secrets (RESTIC_PASSWORD, cloud credentials, other targets' password envs).
const hookEnvPrefix = "SABON_HOOK_ENV_"

// expandEnv turns a KEY->VALUE map into KEY=VALUE strings, expanding ${NAME} in
// the values from the labelled container's own environment first (the label
// author owns it), then falling back to the SABON_HOOK_ENV_<NAME> allowlist.
// sabon's full environment is never exposed.
func expandEnv(m, containerEnv map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	env := make([]string, 0, len(m))
	for k, v := range m {
		env = append(env, k+"="+os.Expand(v, func(name string) string {
			if val, ok := containerEnv[name]; ok {
				return val
			}
			return os.Getenv(hookEnvPrefix + name)
		}))
	}
	return env
}
