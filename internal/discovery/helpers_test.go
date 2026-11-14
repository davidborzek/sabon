package discovery

import (
	"errors"
	"strings"
	"testing"
)

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

func TestValidateOwnership(t *testing.T) {
	jobs := []Job{
		{App: "a", Container: "one"},
		{App: "shared", Container: "two"},
		{App: "b", Container: "three"},
		{App: "shared", Container: "four", Backup: "data"},
	}
	err := ValidateOwnership(jobs)
	if !errors.Is(err, ErrOwnership) {
		t.Fatalf("want ownership error, got %v", err)
	}
	for _, text := range []string{"shared", "two (plain backup)", "four (backup data)", "distinct repo"} {
		if !strings.Contains(err.Error(), text) {
			t.Errorf("missing %q in %v", text, err)
		}
	}
}

func TestValidateOwnershipRejectsSingletonSiblings(t *testing.T) {
	jobs := []Job{{App: "app", Container: "server"}, {App: "app", Container: "db"}, {App: "b"}}
	if err := ValidateOwnership(jobs); !errors.Is(err, ErrOwnership) {
		t.Errorf("want ownership error, got %v", err)
	}
}

func TestValidateOwnershipDistinctAndEmpty(t *testing.T) {
	for _, jobs := range [][]Job{nil, {{App: "a"}, {App: "b"}}} {
		if err := ValidateOwnership(jobs); err != nil {
			t.Fatal(err)
		}
	}
}

func TestValidateOwnershipSameContainerAndStableDiagnostics(t *testing.T) {
	jobs := []Job{{App: "z", Container: "one"}, {App: "z", Container: "one", Backup: "data"}, {App: "a", Container: "two"}, {App: "a", Container: "three"}}
	want := ValidateOwnership(jobs).Error()
	for i, j := 0, len(jobs)-1; i < j; i, j = i+1, j-1 {
		jobs[i], jobs[j] = jobs[j], jobs[i]
	}
	if got := ValidateOwnership(jobs).Error(); got != want {
		t.Fatalf("unstable errors: %s / %s", got, want)
	}
	if !strings.Contains(want, `"a"`) || !strings.Contains(want, `"z"`) {
		t.Fatalf("missing conflict: %s", want)
	}
}
