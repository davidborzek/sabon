package mover

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

// LabelKey marks a container as a sabon-managed mover so orphans can be reaped.
const LabelKey = "sabon.mover"

// Labels recording a mover run, used for run history (the API reads them back)
// and for history-aware reaping.
const (
	LabelApp    = "sabon.app"
	LabelTarget = "sabon.target"
	LabelAction = "sabon.action"
)

// Request is everything a runtime needs to run one mover. It is the shared
// Docker/Swarm mover contract, expressed in Docker's vocabulary: Mounts/Binds
// are Docker mounts (the swarm engine translates them into service mounts), and
// Node is honoured only by the swarm engine (as a placement constraint).
type Request struct {
	Name           string            // container name (unique per run)
	Image          string            // mover image (sabon's own image by default)
	User           string            // uid[:gid] to run as (movers read arbitrary data -> root)
	Groups         []string          // extra supplementary groups (GIDs) added via --group-add
	Network        string            // optional docker network to attach
	Spec           Spec              // JSON-encoded into SABON_MOVER_SPEC
	Env            []string          // extra KEY=VALUE (RESTIC_*, AWS_*, …)
	Mounts         []mount.Mount     // sources (ro), cache (rw) as typed mounts
	Binds          []string          // legacy binds (local repo, auto-created by the daemon)
	Stdout         io.Writer         // when set, stream mover logs here live (snapshots/restore)
	Labels         map[string]string // extra labels (LabelKey is always set)
	KeepOnShutdown bool              // leave the mover running if sabon is shutting down (not for cold backups)
	Retain         bool              // keep the container after it exits (run history); reap trims per group
	Node           string            // swarm: node hostname to pin the mover to (empty = no placement constraint)
}

// Runner spawns and supervises mover containers.
type Runner struct {
	cli client.APIClient
}

// NewRunner returns a Runner using the given Docker client.
func NewRunner(cli client.APIClient) *Runner { return &Runner{cli: cli} }

// Run creates, starts and waits for one mover, returns its parsed Result, and
// always removes the container. The mover's logs are attached to any error.
func (r *Runner) Run(ctx context.Context, req Request) (Result, bool, error) {
	specJSON, err := MarshalSpec(req.Spec)
	if err != nil {
		return Result{}, false, err
	}
	if err := r.ensureImage(ctx, req.Image); err != nil {
		return Result{}, false, err
	}

	labels := map[string]string{LabelKey: req.Spec.App}
	for k, v := range req.Labels {
		labels[k] = v
	}

	cfg := &container.Config{
		Image:  req.Image,
		User:   req.User,
		Cmd:    []string{"mover"},
		Env:    append([]string{SpecEnv + "=" + specJSON}, req.Env...),
		Labels: labels,
		// A mover serves no health endpoint, so the image's HEALTHCHECK must not apply.
		Healthcheck: &container.HealthConfig{Test: []string{"NONE"}},
	}
	host := &container.HostConfig{
		Mounts:     req.Mounts,
		Binds:      req.Binds,
		GroupAdd:   req.Groups,
		AutoRemove: false,
	}
	if req.Network != "" {
		host.NetworkMode = container.NetworkMode(req.Network)
	}

	created, err := r.cli.ContainerCreate(ctx, cfg, host, nil, nil, req.Name)
	if err != nil {
		return Result{}, false, fmt.Errorf("create mover: %w", err)
	}
	id := created.ID
	// Remove the mover on exit, EXCEPT when the caller opted into KeepOnShutdown
	// and the context was cancelled (sabon shutting down): then leave it running
	// so the backup finishes on its own, and a later reap removes it once exited.
	keep := false
	defer func() {
		if keep {
			return
		}
		_ = r.cli.ContainerRemove(context.WithoutCancel(ctx), id, container.RemoveOptions{Force: true})
	}()

	if err := r.cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		return Result{}, false, fmt.Errorf("start mover: %w", err)
	}

	// When a live output sink is set (snapshots/restore), stream the mover's
	// logs to it as they happen.
	var streamDone chan struct{}
	if req.Stdout != nil {
		streamDone = make(chan struct{})
		go func() {
			defer close(streamDone)
			rc, err := r.cli.ContainerLogs(ctx, id, container.LogsOptions{ShowStdout: true, ShowStderr: true, Follow: true})
			if err != nil {
				return
			}
			defer func() { _ = rc.Close() }()
			_, _ = stdcopy.StdCopy(req.Stdout, req.Stdout, rc)
		}()
	}

	statusCh, errCh := r.cli.ContainerWait(ctx, id, container.WaitConditionNotRunning)
	var code int64
	select {
	case <-ctx.Done():
		// On a backup deadline (timeout) the mover is stuck → let it be removed.
		// On shutdown (plain cancel) leave it running only if the caller allows
		// it; cold backups must not be kept, as the app is restarted on return.
		if context.Cause(ctx) != context.DeadlineExceeded && req.KeepOnShutdown {
			keep = true
		}
		return Result{}, keep, context.Cause(ctx)
	case err := <-errCh:
		return Result{}, false, fmt.Errorf("wait mover: %w", err)
	case st := <-statusCh:
		code = st.StatusCode
		if st.Error != nil {
			return Result{}, false, fmt.Errorf("mover wait error: %s", st.Error.Message)
		}
	}
	// Keep a retained mover after it exits so its status and logs remain
	// available as run history; the reap trims each group to SABON_MOVER_HISTORY.
	if req.Retain {
		keep = true
	}

	// Live-streamed modes: logs already went to req.Stdout.
	if streamDone != nil {
		<-streamDone
		if code != 0 {
			return Result{}, false, fmt.Errorf("mover exited with code %d", code)
		}
		return Result{}, false, nil
	}

	logs := r.logs(ctx, id)
	res, _ := ParseResult(logs)
	if code != 0 {
		return res, false, fmt.Errorf("mover exited with code %d:\n%s", code, tail(logs, 2000))
	}
	return res, false, nil
}

// logs reads and demultiplexes the mover's stdout+stderr into a single string.
func (r *Runner) logs(ctx context.Context, id string) string {
	rc, err := r.cli.ContainerLogs(ctx, id, container.LogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		return ""
	}
	defer func() { _ = rc.Close() }()
	var buf bytes.Buffer
	if _, err := stdcopy.StdCopy(&buf, &buf, rc); err != nil {
		return buf.String()
	}
	return buf.String()
}

// ensureImage pulls the mover image if it is not present locally.
func (r *Runner) ensureImage(ctx context.Context, ref string) error {
	if _, err := r.cli.ImageInspect(ctx, ref); err == nil {
		return nil
	}
	rc, err := r.cli.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("pull mover image %s: %w", ref, err)
	}
	defer func() { _ = rc.Close() }()
	_, _ = io.Copy(io.Discard, rc)
	return nil
}

// Reap trims exited mover containers, keeping the newest `keep` per
// (app, target, action) group as run history and removing the rest. Ephemeral
// movers with no action label (snapshot/read leftovers) are removed once
// exited. Running movers are always spared. keep <= 0 removes all exited movers.
func (r *Runner) Reap(ctx context.Context, keep int) (int, error) {
	list, err := r.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", LabelKey)),
	})
	if err != nil {
		return 0, err
	}
	groups := map[string][]container.Summary{}
	var stray []container.Summary
	for _, c := range list {
		if c.State == "running" || c.State == "restarting" {
			continue
		}
		if c.Labels[LabelAction] == "" {
			stray = append(stray, c)
			continue
		}
		key := c.Labels[LabelApp] + "|" + c.Labels[LabelTarget] + "|" + c.Labels[LabelAction]
		groups[key] = append(groups[key], c)
	}
	n := 0
	remove := func(id string) {
		if err := r.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true}); err == nil {
			n++
		}
	}
	for _, c := range stray {
		remove(c.ID)
	}
	for _, cs := range groups {
		sort.Slice(cs, func(i, j int) bool { return cs[i].Created > cs[j].Created })
		for i, c := range cs {
			if i >= keep {
				remove(c.ID)
			}
		}
	}
	return n, nil
}

// DetectSelfImage returns the image reference of sabon's own container so movers
// can reuse it (single image, single restic version). Works only when running
// inside Docker.
func DetectSelfImage(ctx context.Context, cli client.APIClient) (string, error) {
	host, err := os.Hostname()
	if err != nil {
		return "", err
	}
	insp, err := cli.ContainerInspect(ctx, host)
	if err != nil {
		return "", fmt.Errorf("inspect self (%s): %w", host, err)
	}
	if insp.Config != nil && insp.Config.Image != "" {
		return insp.Config.Image, nil
	}
	return insp.Image, nil
}

// MarshalSpec encodes a Spec as the JSON carried in SABON_MOVER_SPEC.
func MarshalSpec(s Spec) (string, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("marshal mover spec: %w", err)
	}
	return string(b), nil
}

func tail(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
