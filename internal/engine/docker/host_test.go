package docker

import (
	"strings"
	"testing"

	"github.com/docker/docker/api/types/volume"
)

func TestNonLocalBacking(t *testing.T) {
	cases := []struct {
		name    string
		vol     volume.Volume
		foreign bool
		substr  string
	}{
		{"plain local dir", volume.Volume{Driver: "local"}, false, ""},
		{"plugin driver", volume.Volume{Driver: "rexray"}, true, "rexray"},
		{"local nfs mount", volume.Volume{Driver: "local", Options: map[string]string{"type": "nfs", "device": ":/export"}}, true, "nfs"},
		{"local opts without type", volume.Volume{Driver: "local", Options: map[string]string{"o": "bind"}}, true, "not local storage"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reason, ok := nonLocalBacking(c.vol)
			if ok != c.foreign {
				t.Errorf("nonLocalBacking(%+v) ok=%v, want %v (reason %q)", c.vol, ok, c.foreign, reason)
			}
			if c.foreign && !strings.Contains(reason, c.substr) {
				t.Errorf("reason %q must contain %q", reason, c.substr)
			}
		})
	}
}
