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

func TestDropNamedCollisions(t *testing.T) {
	jobs := []Job{
		{App: "a", Container: "one"},
		{App: "shared", Container: "two"},
		{App: "b", Container: "three"},
		{App: "shared", Container: "four", Backup: "data"},
	}
	kept, collisions := DropNamedCollisions(jobs)
	if len(kept) != 2 || kept[0].App != "a" || kept[1].App != "b" {
		t.Errorf("kept = %+v, want [a b]: a repo shared with a named backup must back up from neither", kept)
	}
	if got := collisions["shared"]; len(got) != 2 || got[0] != "two" || got[1] != "four" {
		t.Errorf("collisions[shared] = %v, want [two four]", got)
	}
	if len(collisions) != 1 {
		t.Errorf("collisions = %v, want only shared", collisions)
	}
}

// Sibling containers sharing one repo through plain <prefix>.backup labels are
// a documented pattern and keep their existing behaviour.
func TestDropNamedCollisionsKeepsSingletonSiblings(t *testing.T) {
	jobs := []Job{{App: "app", Container: "server"}, {App: "app", Container: "db"}, {App: "b"}}
	kept, collisions := DropNamedCollisions(jobs)
	if len(kept) != 3 || len(collisions) != 0 {
		t.Errorf("kept=%v collisions=%v, want all kept and none reported", kept, collisions)
	}
}
