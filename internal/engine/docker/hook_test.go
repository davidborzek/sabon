package docker

import (
	"testing"

	"github.com/docker/docker/api/types/mount"
)

func TestParseMounts(t *testing.T) {
	ms, err := parseMounts([]string{"dumps:/dump", "/srv/data:/data:ro"})
	if err != nil {
		t.Fatalf("parseMounts: %v", err)
	}
	if len(ms) != 2 {
		t.Fatalf("want 2 mounts, got %d", len(ms))
	}
	if ms[0].Type != mount.TypeVolume || ms[0].Source != "dumps" || ms[0].Target != "/dump" || ms[0].ReadOnly {
		t.Errorf("volume mount wrong: %+v", ms[0])
	}
	if ms[1].Type != mount.TypeBind || ms[1].Source != "/srv/data" || ms[1].Target != "/data" || !ms[1].ReadOnly {
		t.Errorf("bind mount wrong: %+v", ms[1])
	}
}

func TestParseMountsInvalid(t *testing.T) {
	for _, v := range []string{"onlyone", "a:/b:rw", "a:/b:c:d"} {
		if _, err := parseMounts([]string{v}); err == nil {
			t.Errorf("expected error for %q", v)
		}
	}
}
