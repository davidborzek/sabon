package zfs

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

// snapshotterLabel marks a container as a sabon zfs-snapshot snapshotter so orphans can
// be reaped after a crash.
const snapshotterLabel = "sabon.zfs-snapshotter"

// dockerRunner runs `zfs <args>` inside an ephemeral privileged snapshotter. The
// *image* owns the transport: its entrypoint turns the args into a host `zfs`
// call (our default snapshotter does so via nsenter; an image bundling zfs would
// just be ENTRYPOINT ["zfs"]). So sabon passes only the zfs subcommand args and
// stays mechanism-agnostic. Privileged + host PID namespace cover both the
// nsenter and bundled-zfs transports.
type dockerRunner struct {
	cli   client.APIClient
	image string
	log   *slog.Logger
}

func newDockerRunner(cli client.APIClient, image string, log *slog.Logger) *dockerRunner {
	return &dockerRunner{cli: cli, image: image, log: log}
}

func (r *dockerRunner) run(ctx context.Context, args ...string) (string, error) {
	if r.image == "" {
		return "", fmt.Errorf("no zfs snapshotter image configured; set SABON_SNAPSHOT_ZFS_IMAGE")
	}
	r.log.Debug("running zfs", "args", args)
	if err := r.ensureImage(ctx); err != nil {
		return "", err
	}
	cfg := &container.Config{Image: r.image, Cmd: args, Labels: map[string]string{snapshotterLabel: "true"}}
	host := &container.HostConfig{Privileged: true, PidMode: "host"}
	created, err := r.cli.ContainerCreate(ctx, cfg, host, nil, nil, "")
	if err != nil {
		return "", fmt.Errorf("create zfs snapshotter: %w", err)
	}
	id := created.ID
	defer func() {
		_ = r.cli.ContainerRemove(context.WithoutCancel(ctx), id, container.RemoveOptions{Force: true})
	}()

	if err := r.cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		return "", fmt.Errorf("start zfs snapshotter: %w", err)
	}
	stdout, stderr, err := r.waitAndCollect(ctx, id)
	if err != nil {
		return "", fmt.Errorf("zfs %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr))
	}
	return stdout, nil
}

// waitAndCollect blocks until the snapshotter exits, then returns its stdout and
// stderr. A non-zero exit code is reported as an error.
func (r *dockerRunner) waitAndCollect(ctx context.Context, id string) (stdout, stderr string, err error) {
	statusCh, errCh := r.cli.ContainerWait(ctx, id, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		return "", "", err
	case st := <-statusCh:
		stdout, stderr = r.logs(ctx, id)
		if st.StatusCode != 0 {
			return stdout, stderr, fmt.Errorf("exit %d", st.StatusCode)
		}
		return stdout, stderr, nil
	case <-ctx.Done():
		return "", "", ctx.Err()
	}
}

// ensureImage pulls the snapshotter image if it is not present locally.
func (r *dockerRunner) ensureImage(ctx context.Context) error {
	if _, err := r.cli.ImageInspect(ctx, r.image); err == nil {
		return nil
	}
	r.log.Debug("pulling zfs snapshotter image", "image", r.image)
	rc, err := r.cli.ImagePull(ctx, r.image, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("pull %s: %w", r.image, err)
	}
	defer func() { _ = rc.Close() }()
	_, _ = io.Copy(io.Discard, rc)
	return nil
}

// logs returns the snapshotter's stdout and stderr, demultiplexed.
func (r *dockerRunner) logs(ctx context.Context, id string) (string, string) {
	rc, err := r.cli.ContainerLogs(ctx, id, container.LogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		return "", ""
	}
	defer func() { _ = rc.Close() }()
	var out, errb strings.Builder
	_, _ = stdcopy.StdCopy(&out, &errb, rc)
	return out.String(), errb.String()
}

// reap removes exited leftover snapshotter containers from a crash. A running snapshotter
// (an in-flight backup's snapshot call) is left to finish and reaped next time.
func (r *dockerRunner) reap(ctx context.Context) (int, error) {
	list, err := r.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", snapshotterLabel)),
	})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, c := range list {
		if c.State == "running" || c.State == "restarting" {
			continue
		}
		if err := r.cli.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true}); err == nil {
			n++
		}
	}
	return n, nil
}
