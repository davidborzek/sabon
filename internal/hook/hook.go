// Package hook runs pre/post backup actions against the Docker engine. A hook
// runs in one of two modes: exec (into an existing container) or run (a fresh
// one-shot container from a given image). It also handles stop/start for cold
// backups.
package hook

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

// Runner executes hooks against the Docker engine.
type Runner struct {
	cli client.APIClient
}

// New returns a hook Runner.
func New(cli client.APIClient) *Runner { return &Runner{cli: cli} }

// LabelKey marks one-shot hook containers so crashed/aborted ones can be reaped.
const LabelKey = "sabon.hook"

// Exec runs a command inside an existing container (exec mode) and returns an
// error, with captured output, if it cannot start or exits non-zero.
func (r *Runner) Exec(ctx context.Context, containerID string, cmd, env []string, user string) error {
	if len(cmd) == 0 {
		return fmt.Errorf("hook: empty command")
	}
	create, err := r.cli.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		Cmd:          cmd,
		Env:          env,
		User:         user,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return fmt.Errorf("exec create in %s: %w", containerID, err)
	}
	att, err := r.cli.ContainerExecAttach(ctx, create.ID, container.ExecAttachOptions{})
	if err != nil {
		return fmt.Errorf("exec attach in %s: %w", containerID, err)
	}
	defer att.Close()

	// StdCopy over the hijacked exec stream does not observe ctx; close the
	// connection on cancel/timeout so it unblocks and we surface the ctx error
	// instead of wedging the caller's per-repository lock forever.
	streamDone := make(chan struct{})
	defer close(streamDone)
	go func() {
		select {
		case <-ctx.Done():
			att.Close()
		case <-streamDone:
		}
	}()

	var out bytes.Buffer
	if _, err := stdcopy.StdCopy(&out, &out, att.Reader); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("exec in %s: %w", containerID, ctx.Err())
		}
		return fmt.Errorf("exec stream in %s: %w", containerID, err)
	}
	if ctx.Err() != nil {
		return fmt.Errorf("exec in %s: %w", containerID, ctx.Err())
	}
	insp, err := r.cli.ContainerExecInspect(ctx, create.ID)
	if err != nil {
		return fmt.Errorf("exec inspect in %s: %w", containerID, err)
	}
	if insp.ExitCode != 0 {
		return fmt.Errorf("hook in %s exited with code %d: %s", containerID, insp.ExitCode, out.String())
	}
	return nil
}

// RunSpec configures a one-shot container hook (run mode).
type RunSpec struct {
	Name    string
	Image   string
	Command []string
	User    string
	Env     []string
	Network string
	Volumes []string // "source:/target[:ro]"; source = volume name or host path
}

// RunOneShot spawns a fresh container from spec.Image, runs the command, waits
// for it, and removes it. Non-zero exit (with logs) is an error.
func (r *Runner) RunOneShot(ctx context.Context, spec RunSpec) error {
	if spec.Image == "" {
		return fmt.Errorf("hook: run mode needs an image")
	}
	mounts, err := parseMounts(spec.Volumes)
	if err != nil {
		return err
	}
	if err := r.ensureImage(ctx, spec.Image); err != nil {
		return err
	}

	cfg := &container.Config{
		Image:  spec.Image,
		Cmd:    spec.Command,
		User:   spec.User,
		Env:    spec.Env,
		Labels: map[string]string{LabelKey: spec.Name},
	}
	host := &container.HostConfig{Mounts: mounts}
	if spec.Network != "" {
		host.NetworkMode = container.NetworkMode(spec.Network)
	}

	created, err := r.cli.ContainerCreate(ctx, cfg, host, nil, nil, spec.Name)
	if err != nil {
		return fmt.Errorf("create hook container: %w", err)
	}
	id := created.ID
	defer func() {
		_ = r.cli.ContainerRemove(context.WithoutCancel(ctx), id, container.RemoveOptions{Force: true})
	}()

	if err := r.cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		return fmt.Errorf("start hook container: %w", err)
	}
	statusCh, errCh := r.cli.ContainerWait(ctx, id, container.WaitConditionNotRunning)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		return fmt.Errorf("wait hook container: %w", err)
	case st := <-statusCh:
		if st.StatusCode != 0 {
			return fmt.Errorf("hook container %s exited with code %d:\n%s", spec.Image, st.StatusCode, r.logs(ctx, id))
		}
	}
	return nil
}

// Stop stops a container (used for cold backups).
func (r *Runner) Stop(ctx context.Context, containerID string) error {
	if err := r.cli.ContainerStop(ctx, containerID, container.StopOptions{}); err != nil {
		return fmt.Errorf("stop %s: %w", containerID, err)
	}
	return nil
}

// Start (re)starts a container.
func (r *Runner) Start(ctx context.Context, containerID string) error {
	if err := r.cli.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		return fmt.Errorf("start %s: %w", containerID, err)
	}
	return nil
}

// Reap removes EXITED leftover one-shot hook containers (from a crash or an
// aborted run). Running ones are left alone. Returns how many were removed.
func (r *Runner) Reap(ctx context.Context) (int, error) {
	list, err := r.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", LabelKey), filters.Arg("status", "exited")),
	})
	if err != nil {
		return 0, fmt.Errorf("list hook containers: %w", err)
	}
	n := 0
	for _, c := range list {
		if err := r.cli.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true}); err == nil {
			n++
		}
	}
	return n, nil
}

// ContainerProject returns a container's Docker Compose project label, or "".
func (r *Runner) ContainerProject(ctx context.Context, id string) (string, error) {
	insp, err := r.cli.ContainerInspect(ctx, id)
	if err != nil {
		return "", err
	}
	return insp.Config.Labels["com.docker.compose.project"], nil
}

func (r *Runner) logs(ctx context.Context, id string) string {
	rc, err := r.cli.ContainerLogs(ctx, id, container.LogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		return ""
	}
	defer func() { _ = rc.Close() }()
	var buf bytes.Buffer
	_, _ = stdcopy.StdCopy(&buf, &buf, rc)
	return buf.String()
}

func (r *Runner) ensureImage(ctx context.Context, ref string) error {
	if _, err := r.cli.ImageInspect(ctx, ref); err == nil {
		return nil
	}
	rc, err := r.cli.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("pull hook image %s: %w", ref, err)
	}
	defer func() { _ = rc.Close() }()
	_, _ = io.Copy(io.Discard, rc)
	return nil
}

// parseMounts turns "source:/target[:ro]" strings into Docker mounts. A source
// beginning with "/" is a bind mount; otherwise it is a named volume.
func parseMounts(vols []string) ([]mount.Mount, error) {
	var ms []mount.Mount
	for _, v := range vols {
		parts := strings.Split(v, ":")
		if len(parts) < 2 || len(parts) > 3 {
			return nil, fmt.Errorf("hook volume %q: want source:/target[:ro]", v)
		}
		m := mount.Mount{Source: parts[0], Target: parts[1]}
		if strings.HasPrefix(parts[0], "/") {
			m.Type = mount.TypeBind
		} else {
			m.Type = mount.TypeVolume
		}
		if len(parts) == 3 {
			if parts[2] != "ro" {
				return nil, fmt.Errorf("hook volume %q: third field must be 'ro'", v)
			}
			m.ReadOnly = true
		}
		ms = append(ms, m)
	}
	return ms, nil
}

// ContainerEnv returns a container's environment as a KEY->VALUE map.
func (r *Runner) ContainerEnv(ctx context.Context, id string) (map[string]string, error) {
	insp, err := r.cli.ContainerInspect(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(insp.Config.Env))
	for _, kv := range insp.Config.Env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[k] = v
		}
	}
	return out, nil
}
