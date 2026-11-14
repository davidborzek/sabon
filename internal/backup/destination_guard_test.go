package backup

import (
	"testing"

	"github.com/davidborzek/sabon/api"
	"github.com/davidborzek/sabon/internal/config"
	"github.com/davidborzek/sabon/internal/discovery"
	"github.com/docker/docker/api/types/mount"
)

func TestGuardedLocalRepoUsesStrictBind(t *testing.T) {
	target := api.Target{Path: "/backup", DestinationMarker: "disk"}
	o := &Orchestrator{cfg: &config.Config{CacheVolume: "cache"}}
	mounts, binds := o.cacheAndRepo(target, "demo")
	if len(binds) != 0 {
		t.Fatalf("directory-creating binds: %v", binds)
	}
	found := false
	for _, m := range mounts {
		if m.Target == repoMount {
			found = true
			if m.Type != mount.TypeBind || m.Source != "/backup/demo" || m.ReadOnly || m.BindOptions != nil {
				t.Fatalf("wrong repo mount: %+v", m)
			}
		}
	}
	if !found {
		t.Fatal("repository mount missing")
	}
	if spec := o.moverSpec(discovery.Job{App: "demo"}, target); spec.DestinationMarker != target.DestinationMarker {
		t.Fatalf("marker not propagated: %+v", spec)
	}
}
