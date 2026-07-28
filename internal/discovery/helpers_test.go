package discovery

import "testing"

func TestNamerDedup(t *testing.T) {
	n := NewNamer()
	if a, b := n.Pick("data"), n.Pick("data"); a == b {
		t.Errorf("namer returned duplicate: %q == %q", a, b)
	}
}

func TestValidAppName(t *testing.T) {
	for _, ok := range []string{"immich", "paperless-ngx", "app_1"} {
		if err := ValidAppName(ok); err != nil {
			t.Errorf("%q should be valid: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "../etc", "a/b", `a\b`, "..", "x/.."} {
		if err := ValidAppName(bad); err == nil {
			t.Errorf("%q should be rejected as a repo name", bad)
		}
	}
}
