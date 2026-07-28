package swarm

// Swarm discovery: sabon-labelled Swarm *services* (labels on the service, i.e.
// compose `deploy.labels`) resolved into discovery.Jobs, each pinned to the
// node its task currently runs on. Named volumes/binds are node-local, so a Job
// is only consistent on that node — hence Node routing into the mover's
// placement.
//
// NOTE: unit-tested against a fake swarm client; NOT validated here against a
// live cluster (CI e2e is the gate). A scaled-to-0 service has no running task,
// so its node is derived from a node.hostname/node.id/node.labels placement
// constraint if present; only one with neither is left unconstrained. ZFS
// snapshots are unsupported in swarm (the snapshotter is node-local).

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/davidborzek/sabon/internal/discovery"
	"github.com/davidborzek/sabon/internal/engine"
	"github.com/davidborzek/sabon/internal/label"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/swarm"
)

var _ engine.Discoverer = (*Discoverer)(nil)

// Discoverer lists sabon-labelled services across the cluster and resolves each
// into a Job pinned to the node its task runs on.
type Discoverer struct {
	cli            swarmClient
	prefix         string
	watchByDefault bool
	cacheVolume    string
	instance       string
	log            *slog.Logger
}

// NewDiscoverer returns a swarm Discoverer. instance, when non-empty, restricts
// discovery to services whose `<prefix>.instance` matches.
func NewDiscoverer(cli swarmClient, prefix string, watchByDefault bool, cacheVolume, instance string, log *slog.Logger) *Discoverer {
	return &Discoverer{cli: cli, prefix: prefix, watchByDefault: watchByDefault, cacheVolume: cacheVolume, instance: instance, log: log}
}

// List returns every enabled, spec-carrying service as a resolved Job. Unlike
// containers, a service with zero replicas still exists, so cold (scaled-down)
// apps are still discovered — but without a running task their Node is empty.
func (d *Discoverer) List(ctx context.Context) ([]discovery.Job, error) {
	svcs, err := d.cli.ServiceList(ctx, swarm.ServiceListOptions{})
	if err != nil {
		return nil, err
	}
	nodes := d.listNodes(ctx)
	var jobs []discovery.Job
	for _, svc := range svcs {
		labels := svc.Spec.Labels
		res, err := label.Read(labels, d.prefix, d.watchByDefault)
		if err != nil {
			d.log.Warn("invalid backup labels", "service", svc.Spec.Name, "error", err)
			continue
		}
		if !res.Enabled || !res.HasSpec {
			continue
		}
		if d.instance != "" && res.Instance != d.instance {
			continue // owned by a different sabon instance
		}
		job, err := d.resolve(ctx, svc, res, nodes)
		if err != nil {
			d.log.Warn("cannot resolve backup sources", "service", svc.Spec.Name, "error", err)
			continue
		}
		if len(job.Sources) == 0 {
			d.log.Warn("backup enabled but no sources resolved", "service", svc.Spec.Name, "app", job.App)
			continue
		}
		jobs = append(jobs, job)
		d.log.Debug("discovered job", "app", job.App, "service", svc.Spec.Name, "node", job.Node, "sources", len(job.Sources))
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].App < jobs[j].App })
	return jobs, nil
}

// resolve turns a service + its spec into a Job with concrete sources, taken
// from the service's ContainerSpec mounts (auto) plus the spec's extras.
func (d *Discoverer) resolve(ctx context.Context, svc swarm.Service, res label.Result, nodes []swarm.Node) (discovery.Job, error) {
	sname := svc.Spec.Name
	labels := svc.Spec.Labels
	app := res.Spec.Repo
	if app == "" {
		if s := labels[discovery.ComposeServiceLabel]; s != "" {
			app = s
		} else {
			app = sname
		}
	}
	if err := discovery.ValidAppName(app); err != nil {
		return discovery.Job{}, err
	}
	job := discovery.Job{
		Container: sname, // the service ref, used by the quiescer for scale-to-0
		App:       app,
		Instance:  res.Instance,
		Project:   labels[discovery.ComposeProjectLabel],
		Spec:      res.Spec,
		Node:      d.nodeFor(ctx, svc, nodes),
	}
	names := discovery.NewNamer()
	// Auto: the service's own bind mounts and named volumes.
	if cs := svc.Spec.TaskTemplate.ContainerSpec; res.Spec.AutoSources() && cs != nil {
		excluded := make(map[string]struct{}, len(res.Spec.ExcludeVolumes))
		for _, v := range res.Spec.ExcludeVolumes {
			excluded[v] = struct{}{}
		}
		for _, m := range cs.Mounts {
			switch m.Type {
			case mount.TypeBind:
				if m.Source == "" {
					continue
				}
				job.Sources = append(job.Sources, discovery.Source{Name: names.Pick(discovery.BaseName(m.Target, m.Source)), Type: mount.TypeBind, Ref: m.Source})
			case mount.TypeVolume:
				if m.Source == "" || m.Source == d.cacheVolume {
					continue
				}
				if _, skip := excluded[m.Source]; skip {
					continue
				}
				job.Sources = append(job.Sources, discovery.Source{Name: names.Pick(m.Source), Type: mount.TypeVolume, Ref: m.Source})
			}
		}
	}
	// Explicit named volumes.
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

// nodeFor returns the node a job should be pinned to: the node of its running
// task, else the node its placement constraint pins it to (scaled-to-0 case).
func (d *Discoverer) nodeFor(ctx context.Context, svc swarm.Service, nodes []swarm.Node) string {
	if h := d.taskNode(ctx, svc.ID, nodes); h != "" {
		return h
	}
	// No running task (e.g. scaled to zero): fall back to the service's own
	// placement constraint so a pinned service is still backed up on its node.
	return placementNode(svc, nodes)
}

// taskNode returns the hostname of the node running the service's task, or ""
// when there is no running task.
func (d *Discoverer) taskNode(ctx context.Context, serviceID string, nodes []swarm.Node) string {
	tasks, err := d.cli.TaskList(ctx, swarm.TaskListOptions{Filters: filters.NewArgs(
		filters.Arg("service", serviceID),
		filters.Arg("desired-state", "running"),
	)})
	if err != nil {
		d.log.Warn("cannot list service tasks for node placement", "service", serviceID, "error", err)
		return ""
	}
	// Return the first task whose node hostname resolves. An unresolved node
	// (NodeList unavailable, or the node missing) yields "" so nodeFor falls
	// back to the placement constraint — never a raw node id, which would be an
	// unsatisfiable node.hostname== constraint.
	for _, t := range tasks {
		if h := hostByID(nodes, t.NodeID); h != "" {
			return h
		}
	}
	return ""
}

func (d *Discoverer) listNodes(ctx context.Context) []swarm.Node {
	ns, err := d.cli.NodeList(ctx, swarm.NodeListOptions{})
	if err != nil {
		d.log.Warn("cannot list swarm nodes; movers may be unconstrained", "error", err)
		return nil
	}
	return ns
}

func hostByID(nodes []swarm.Node, id string) string {
	for _, n := range nodes {
		if n.ID == id {
			return n.Description.Hostname
		}
	}
	return ""
}

// placementNode derives a target node from a service's placement constraints, so
// a service with no running task (scaled to zero) can still be pinned. It handles
// node.hostname==, node.id== and an unambiguous node.labels.<key>==; other
// constraints (node.role, inequalities, …) do not identify a single node.
func placementNode(svc swarm.Service, nodes []swarm.Node) string {
	if svc.Spec.TaskTemplate.Placement == nil {
		return ""
	}
	for _, c := range svc.Spec.TaskTemplate.Placement.Constraints {
		key, val, ok := constraintEq(c)
		if !ok || val == "" {
			continue
		}
		switch {
		case key == "node.hostname":
			return val
		case key == "node.id":
			if h := hostByID(nodes, val); h != "" {
				return h
			}
		case strings.HasPrefix(key, "node.labels."):
			lk := strings.TrimPrefix(key, "node.labels.")
			match, n := "", 0
			for _, nd := range nodes {
				if nd.Spec.Labels[lk] == val {
					match, n = nd.Description.Hostname, n+1
				}
			}
			if n == 1 { // only when it resolves to exactly one node
				return match
			}
		}
	}
	return ""
}

// constraintEq splits an "a == b" equality constraint, trimming spaces; ok is
// false for anything without "==" (e.g. "!=" inequalities).
func constraintEq(c string) (key, val string, ok bool) {
	i := strings.Index(c, "==")
	if i < 0 {
		return "", "", false
	}
	return strings.TrimSpace(c[:i]), strings.TrimSpace(c[i+2:]), true
}

// Watch emits a signal on any relevant service event, resubscribing on stream
// errors and stopping when ctx is cancelled (mirrors the standalone watcher).
func (d *Discoverer) Watch(ctx context.Context, onRestart func()) <-chan struct{} {
	out := make(chan struct{}, 1)
	go func() {
		defer close(out)
		for {
			f := filters.NewArgs(
				filters.Arg("type", "service"),
				filters.Arg("event", "create"),
				filters.Arg("event", "update"),
				filters.Arg("event", "remove"),
			)
			msgs, errs := d.cli.Events(ctx, events.ListOptions{Filters: f})
			for {
				select {
				case <-ctx.Done():
					return
				case msg := <-msgs:
					d.log.Debug("service event", "action", string(msg.Action), "service", msg.Actor.Attributes["name"])
					discovery.Signal(out)
				case err := <-errs:
					if ctx.Err() != nil {
						return
					}
					if err != nil {
						d.log.Warn("swarm event stream error, resubscribing", "error", err)
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
