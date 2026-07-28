package main

import (
	"context"
	"errors"
	"testing"

	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/api/types/system"
	"github.com/docker/docker/client"
)

// fakeInfoClient satisfies client.APIClient (embedded) but only implements the
// Info call detectRuntime uses.
type fakeInfoClient struct {
	client.APIClient
	control bool
	err     error
}

func (f fakeInfoClient) Info(context.Context) (system.Info, error) {
	if f.err != nil {
		return system.Info{}, f.err
	}
	return system.Info{Swarm: swarm.Info{ControlAvailable: f.control}}, nil
}

func TestDetectRuntime(t *testing.T) {
	ctx := context.Background()
	boom := fakeInfoClient{err: errors.New("boom")}

	// Explicit overrides never touch docker info.
	if m, err := detectRuntime(ctx, boom, "standalone"); err != nil || m != "standalone" {
		t.Errorf("override standalone = %q, %v", m, err)
	}
	if m, err := detectRuntime(ctx, boom, "swarm"); err != nil || m != "swarm" {
		t.Errorf("override swarm = %q, %v", m, err)
	}
	// Auto-detect from swarm control availability.
	if m, err := detectRuntime(ctx, fakeInfoClient{control: true}, "auto"); err != nil || m != "swarm" {
		t.Errorf("auto (manager) = %q, %v", m, err)
	}
	if m, err := detectRuntime(ctx, fakeInfoClient{control: false}, ""); err != nil || m != "standalone" {
		t.Errorf("auto (non-manager) = %q, %v", m, err)
	}
	// Auto with a broken daemon surfaces the error.
	if _, err := detectRuntime(ctx, boom, "auto"); err == nil {
		t.Error("auto with a docker info error must fail")
	}
	// Unknown override is rejected.
	if _, err := detectRuntime(ctx, fakeInfoClient{}, "bogus"); err == nil {
		t.Error("invalid runtime override must fail")
	}
}
