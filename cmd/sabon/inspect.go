package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/davidborzek/sabon/api"
	"github.com/davidborzek/sabon/internal/config"
	"github.com/davidborzek/sabon/internal/mover"
	"github.com/davidborzek/sabon/internal/snapshot"
	"github.com/docker/docker/api/types/mount"
	"github.com/invopop/jsonschema"
	"github.com/urfave/cli/v3"
)

// runMover is the mover-side entrypoint executed inside an ephemeral container.
func runMover(ctx context.Context, cmd *cli.Command) error {
	return mover.Execute(ctx, os.Stdout, os.Stderr)
}

// runSchema prints a JSON schema for either the backup label or the targets file.
func runSchema(ctx context.Context, cmd *cli.Command) error {
	var s *jsonschema.Schema
	if cmd.Bool("targets") {
		s = jsonschema.Reflect(&api.File{})
	} else {
		s = jsonschema.Reflect(&api.Spec{})
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

// runValidate lists discovered jobs and their resolved plan without running.
func runValidate(ctx context.Context, cmd *cli.Command) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := newLogger(cfg.LogLevel)
	cli, err := dockerClient()
	if err != nil {
		return err
	}
	defer func() { _ = cli.Close() }()

	rt, err := newRuntime(ctx, cli, cfg, logger)
	if err != nil {
		return err
	}

	disc := rt.disc
	jobs, err := disc.List(ctx)
	if err != nil {
		return err
	}
	if len(jobs) == 0 {
		fmt.Println("no backup jobs discovered")
		return nil
	}

	orch := newOrchestrator(cfg, "", rt, logger)
	anyZFS := false
	zfsErr := ""
	for _, j := range jobs {
		targets := j.Spec.TargetNames()
		if len(targets) == 0 {
			for _, t := range cfg.Targets {
				targets = append(targets, t.Name)
			}
		}
		mode := j.Spec.SnapshotMode(cfg.Snapshot)
		fmt.Printf("app=%s container=%s targets=%v sources=%d snapshot=%s\n", j.App, j.Container, targets, len(j.Sources), mode)

		res := map[string]snapshot.Resolution{}
		if mode == "zfs" || mode == "auto" {
			anyZFS = true
			if zfsErr == "" {
				rs, err := orch.PreviewSnapshots(ctx, j)
				if err != nil {
					zfsErr = err.Error()
				} else {
					for _, r := range rs {
						res[r.Name] = r
					}
				}
			}
		}
		for _, s := range j.Sources {
			kind := string(s.Type)
			driver := "" // volume driver when known; "" for binds or inspect failure
			if s.Type == mount.TypeVolume {
				disp := "?"
				if v, err := cli.VolumeInspect(ctx, s.Ref); err == nil {
					driver, disp = v.Driver, v.Driver
				}
				kind = "volume, driver=" + disp
			}
			line := fmt.Sprintf("  - %s (%s) %s", s.Name, kind, s.Ref)
			if r, ok := res[s.Name]; ok && r.Detail != "" {
				if mode == "auto" && !r.Snapshottable {
					line += " → live (" + r.Detail + ")"
				} else {
					line += " → " + r.Detail
				}
			}
			fmt.Println(line)
			if h := singleAttachHint(driver, j.Spec.Stop); h != "" {
				fmt.Printf("    note: %s\n", h)
			}
		}
	}
	if anyZFS {
		if zfsErr != "" {
			fmt.Printf("snapshot: zfs unavailable on host (%s) — zfs jobs fail, auto jobs mount live\n", zfsErr)
		} else {
			fmt.Println("snapshot: zfs available")
		}
	}
	return nil
}

// singleAttachHint warns when a source volume uses a non-local driver and the
// app is not cold-backed up: some block drivers attach a volume to a single
// container at a time, so the mover may fail to mount it read-only alongside
// the running app. Returns "" when no hint applies (bind, local, or stop:true).
func singleAttachHint(driver string, stop bool) string {
	if driver == "" || driver == "local" || stop {
		return ""
	}
	return fmt.Sprintf("driver %q may be single-attach; set stop: true if the mover can't mount it", driver)
}
