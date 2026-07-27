package main

import (
	"strings"
	"testing"
)

func TestSingleAttachHint(t *testing.T) {
	cases := []struct {
		name   string
		driver string
		stop   bool
		want   bool // whether a hint is expected
	}{
		{"bind has no driver", "", false, false},
		{"local driver mounts fine", "local", false, false},
		{"non-local driver warns", "rexray", false, true},
		{"non-local but cold backup", "rexray", true, false},
		{"local with stop", "local", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := singleAttachHint(c.driver, c.stop)
			if (got != "") != c.want {
				t.Errorf("singleAttachHint(%q, %v) = %q, want hint=%v", c.driver, c.stop, got, c.want)
			}
			if c.want && !strings.Contains(got, c.driver) {
				t.Errorf("hint must name the driver %q, got %q", c.driver, got)
			}
		})
	}
}
