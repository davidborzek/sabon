// Package discovery finds sabon-labelled containers on the Docker engine,
// resolves each into a backup Job (app name + the concrete sources to back up),
// and watches for lifecycle changes.
package discovery

import (
	"context"
	"fmt"
	"log/slog"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/davidborzek/sabon/api"
	"github.com/davidborzek/sabon/internal/label"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
)

const composeServiceLabel = "com.docker.compose.service"
const composeProjectLabel = "com.docker.compose.project"

// Source is one thing to back up, mounted read-only into the mover at
// /data/<Name>.
type Source struct {
	Name string     // stable, unique mount name (the /data/<Name> segment)
	Type mount.Type // bind or volume
	Ref  string     // host path (bind) or volume name (volume)
}

// Job is a resolved backup unit for one app.
type Job struct {
	Container string   // labelled container name
	App       string   // repository/app name
	Instance  string   // owning sabon instance (from <prefix>.instance)
	Project   string   // labelled container's compose project (scopes exec hooks)
	Spec      api.Spec // parsed backup spec
	Sources   []Source // concrete sources
}

// Discoverer lists and watches sabon-labelled containers.
type Discoverer struct {
	cli            client.APIClient
	prefix         string
	watchByDefault bool
	cacheVolume    string
	instance       string
	log            *slog.Logger
}

// New returns a Discoverer. instance, when non-empty, restricts discovery to
// containers whose `<prefix>.instance` matches (multi-instance ownership).
func New(cli client.APIClient, prefix string, watchByDefault bool, cacheVolume, instance string, log *slog.Logger) *Discoverer {
	return &Discoverer{cli: cli, prefix: prefix, watchByDefault: watchByDefault, cacheVolume: cacheVolume, instance: instance, log: log}
}

// List returns every enabled, spec-carrying container as a resolved Job. It
// includes stopped containers: their volumes still exist and a cold container
// is the most consistent backup state, so stopping a service must not silently
// stop its backups.
func (d *Discoverer) List(ctx context.Context) ([]Job, error) {
	cs, err := d.cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, err
	}
	var jobs []Job
	for _, c := range cs {
		res, err := label.Read(c.Labels, d.prefix, d.watchByDefault)
		if err != nil {
			d.log.Warn("invalid backup labels", "container", name(c), "error", err)
			continue
		}
		if !res.Enabled || !res.HasSpec {
			continue
		}
		if d.instance != "" && res.Instance != d.instance {
			continue // owned by a different sabon instance
		}
		job, err := d.resolve(ctx, name(c), c.Labels, res)
		if err != nil {
			d.log.Warn("cannot resolve backup sources", "container", name(c), "error", err)
			continue
		}
		if len(job.Sources) == 0 {
			d.log.Warn("backup enabled but no sources resolved", "container", name(c), "app", job.App)
			continue
		}
		jobs = append(jobs, job)
		d.log.Debug("discovered job", "app", job.App, "container", name(c), "sources", len(job.Sources))
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].App < jobs[j].App })
	return jobs, nil
}

// resolve turns a container + its spec into a Job with concrete sources.
func (d *Discoverer) resolve(ctx context.Context, cname string, labels map[string]string, res label.Result) (Job, error) {
	app := res.Spec.Repo
	if app == "" {
		if svc := labels[composeServiceLabel]; svc != "" {
			app = svc
		} else {
			app = cname
		}
	}
	if err := validAppName(app); err != nil {
		return Job{}, err
	}
	job := Job{Container: cname, App: app, Instance: res.Instance, Project: labels[composeProjectLabel], Spec: res.Spec}
	names := newNamer()
	// Auto: the container's own bind mounts and named volumes.
	if res.Spec.AutoSources() {
		excluded := make(map[string]struct{}, len(res.Spec.ExcludeVolumes))
		for _, v := range res.Spec.ExcludeVolumes {
			excluded[v] = struct{}{}
		}
		insp, err := d.cli.ContainerInspect(ctx, cname)
		if err != nil {
			return Job{}, err
		}
		for _, m := range insp.Mounts {
			switch m.Type {
			case mount.TypeBind:
				if m.Source == "" {
					continue
				}
				job.Sources = append(job.Sources, Source{
					Name: names.pick(baseName(m.Destination, m.Source)),
					Type: mount.TypeBind,
					Ref:  m.Source,
				})
			case mount.TypeVolume:
				if m.Name == "" || m.Name == d.cacheVolume {
					continue
				}
				if _, skip := excluded[m.Name]; skip {
					continue
				}
				job.Sources = append(job.Sources, Source{
					Name: names.pick(m.Name),
					Type: mount.TypeVolume,
					Ref:  m.Name,
				})
			}
		}
	}
	// Explicit named volumes (may belong to sibling containers of the app).
	for _, v := range res.Spec.ExtraVolumes {
		if v == "" {
			continue
		}
		job.Sources = append(job.Sources, Source{Name: names.pick(v), Type: mount.TypeVolume, Ref: v})
	}
	// Explicit host paths.
	for _, p := range res.Spec.ExtraPaths {
		if p == "" {
			continue
		}
		job.Sources = append(job.Sources, Source{Name: names.pick(baseName(p, p)), Type: mount.TypeBind, Ref: p})
	}
	return job, nil
}

// validAppName rejects app/repo names that could escape a target's base path
// once joined into it (or into a remote {app} template).
func validAppName(app string) error {
	if app == "" {
		return fmt.Errorf("empty app/repo name")
	}
	if strings.ContainsAny(app, `/\`) || strings.Contains(app, "..") {
		return fmt.Errorf("app/repo name %q must not contain path separators or %q", app, "..")
	}
	return nil
}

// Watch emits a signal whenever a relevant container event occurs. It
// resubscribes on stream errors and stops when ctx is cancelled.
func (d *Discoverer) Watch(ctx context.Context, onRestart func()) <-chan struct{} {
	out := make(chan struct{}, 1)
	go func() {
		defer close(out)
		for {
			f := filters.NewArgs(
				filters.Arg("type", "container"),
				filters.Arg("event", "start"),
				filters.Arg("event", "die"),
				filters.Arg("event", "destroy"),
				filters.Arg("event", "update"),
			)
			msgs, errs := d.cli.Events(ctx, events.ListOptions{Filters: f})
			for {
				select {
				case <-ctx.Done():
					return
				case msg := <-msgs:
					d.log.Debug("container event", "action", string(msg.Action), "container", msg.Actor.Attributes["name"])
					signal(out)
				case err := <-errs:
					if ctx.Err() != nil {
						return
					}
					if err != nil {
						d.log.Warn("docker event stream error, resubscribing", "error", err)
					}
					if onRestart != nil {
						onRestart()
					}
					time.Sleep(time.Second)
					goto resubscribe
				}
			}
		resubscribe:
		}
	}()
	return out
}

func signal(ch chan<- struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func name(c container.Summary) string {
	if len(c.Names) > 0 {
		return strings.TrimPrefix(c.Names[0], "/")
	}
	if len(c.ID) >= 12 {
		return c.ID[:12]
	}
	return c.ID
}

var unsafeName = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// baseName derives a readable mount name, preferring the destination path.
func baseName(dest, src string) string {
	b := path.Base(strings.TrimRight(dest, "/"))
	if b == "" || b == "/" || b == "." {
		b = path.Base(strings.TrimRight(src, "/"))
	}
	b = unsafeName.ReplaceAllString(b, "-")
	b = strings.Trim(b, "-._")
	if b == "" {
		b = "data"
	}
	return b
}

// namer hands out unique, filesystem-safe names, suffixing collisions and
// reserving every name it returns so a generated suffix cannot later collide.
type namer struct{ seen map[string]bool }

func newNamer() *namer { return &namer{seen: map[string]bool{}} }

func (n *namer) pick(base string) string {
	base = unsafeName.ReplaceAllString(base, "-")
	if base == "" {
		base = "data"
	}
	name := base
	for i := 1; n.seen[name]; i++ {
		name = base + "-" + itoa(i)
	}
	n.seen[name] = true
	return name
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	return string(b[p:])
}
