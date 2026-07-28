package docker

// Standalone discovery: sabon-labelled containers on the local daemon, each
// resolved into a discovery.Job.

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/davidborzek/sabon/internal/discovery"
	"github.com/davidborzek/sabon/internal/engine"
	"github.com/davidborzek/sabon/internal/label"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
)

var _ engine.Discoverer = (*Discoverer)(nil)

// Discoverer lists and watches sabon-labelled containers on the local daemon.
type Discoverer struct {
	cli            client.APIClient
	prefix         string
	watchByDefault bool
	cacheVolume    string
	instance       string
	log            *slog.Logger
}

// NewDiscoverer returns a container Discoverer. instance, when non-empty,
// restricts discovery to containers whose `<prefix>.instance` matches.
func NewDiscoverer(cli client.APIClient, prefix string, watchByDefault bool, cacheVolume, instance string, log *slog.Logger) *Discoverer {
	return &Discoverer{cli: cli, prefix: prefix, watchByDefault: watchByDefault, cacheVolume: cacheVolume, instance: instance, log: log}
}

// List returns every enabled, spec-carrying container as a resolved Job. It
// includes stopped containers: their volumes still exist and a cold container
// is the most consistent backup state, so stopping a service must not silently
// stop its backups.
func (d *Discoverer) List(ctx context.Context) ([]discovery.Job, error) {
	cs, err := d.cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, err
	}
	var jobs []discovery.Job
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
func (d *Discoverer) resolve(ctx context.Context, cname string, labels map[string]string, res label.Result) (discovery.Job, error) {
	app := res.Spec.Repo
	if app == "" {
		if svc := labels[discovery.ComposeServiceLabel]; svc != "" {
			app = svc
		} else {
			app = cname
		}
	}
	if err := discovery.ValidAppName(app); err != nil {
		return discovery.Job{}, err
	}
	job := discovery.Job{Container: cname, App: app, Instance: res.Instance, Project: labels[discovery.ComposeProjectLabel], Spec: res.Spec}
	names := discovery.NewNamer()
	// Auto: the container's own bind mounts and named volumes.
	if res.Spec.AutoSources() {
		excluded := make(map[string]struct{}, len(res.Spec.ExcludeVolumes))
		for _, v := range res.Spec.ExcludeVolumes {
			excluded[v] = struct{}{}
		}
		insp, err := d.cli.ContainerInspect(ctx, cname)
		if err != nil {
			return discovery.Job{}, err
		}
		for _, m := range insp.Mounts {
			switch m.Type {
			case mount.TypeBind:
				if m.Source == "" {
					continue
				}
				job.Sources = append(job.Sources, discovery.Source{
					Name: names.Pick(discovery.BaseName(m.Destination, m.Source)),
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
				job.Sources = append(job.Sources, discovery.Source{
					Name: names.Pick(m.Name),
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
		job.Sources = append(job.Sources, discovery.Source{Name: names.Pick(v), Type: mount.TypeVolume, Ref: v})
	}
	// Explicit host paths.
	for _, p := range res.Spec.ExtraPaths {
		if p == "" {
			continue
		}
		job.Sources = append(job.Sources, discovery.Source{Name: names.Pick(discovery.BaseName(p, p)), Type: mount.TypeBind, Ref: p})
	}
	return job, nil
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
					discovery.Signal(out)
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

func name(c container.Summary) string {
	if len(c.Names) > 0 {
		return strings.TrimPrefix(c.Names[0], "/")
	}
	if len(c.ID) >= 12 {
		return c.ID[:12]
	}
	return c.ID
}
