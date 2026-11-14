package api

import (
	"fmt"
	"strings"
)

func (t Target) validateDestinationGuard() error {
	if t.DestinationMarker == "" {
		return nil
	}
	if t.Backend() != BackendLocal {
		return fmt.Errorf("target %q: destinationMarker requires a local path backend", t.Name)
	}
	if strings.TrimSpace(t.DestinationMarker) != t.DestinationMarker {
		return fmt.Errorf("target %q: destinationMarker must not have surrounding whitespace", t.Name)
	}
	for key := range t.Env {
		if strings.HasPrefix(key, "RESTIC_REPOSITORY") || key == "RESTIC_FROM_REPOSITORY" {
			return fmt.Errorf("target %q: destination guard forbids repository overrides in env", t.Name)
		}
	}
	for _, arg := range t.ResticArgs {
		if strings.HasPrefix(arg, "--repo") || strings.HasPrefix(arg, "-r") && !strings.HasPrefix(arg, "--") {
			return fmt.Errorf("target %q: destination guard forbids repository overrides in resticArgs", t.Name)
		}
		// restic accepts clustered short options: e.g. -qr/path selects a
		// different repository after -q. Do not try to replicate its parser
		// (including value-consuming options and future short flags). Require
		// known standalone short flags; long forms support attached values.
		if strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") {
			switch arg {
			case "-q", "-v", "-h", "-o", "-p":
			default:
				return fmt.Errorf("target %q: destination guard requires standalone short flags (-q, -v, -h, -o, -p); use separate values or long options in resticArgs", t.Name)
			}
		}
	}
	return nil
}
