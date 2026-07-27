package label

import (
	"reflect"
	"testing"
)

func TestReadEnableSemantics(t *testing.T) {
	tests := []struct {
		name           string
		labels         map[string]string
		watchByDefault bool
		want           bool
	}{
		{"opt-in off, no label", map[string]string{}, false, false},
		{"opt-in off, enable true", map[string]string{"sabon.enable": "true"}, false, true},
		{"watch-by-default, no label", map[string]string{}, true, true},
		{"watch-by-default, enable false", map[string]string{"sabon.enable": "false"}, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := Read(tt.labels, "sabon", tt.watchByDefault)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Enabled != tt.want {
				t.Fatalf("Enabled = %v, want %v", res.Enabled, tt.want)
			}
		})
	}
}

func TestReadEnableInvalid(t *testing.T) {
	if _, err := Read(map[string]string{"sabon.enable": "yesplease"}, "sabon", false); err == nil {
		t.Fatal("expected error for invalid bool")
	}
}

func TestReadSpec(t *testing.T) {
	doc := "repo: immich\n" +
		"extraVolumes: [immich-db-dump]\n" +
		"extraPaths: [/srv/x]\n" +
		"exclude: [\"**/postgres\"]\n" +
		"targets:\n" +
		"  - onsite\n" +
		"  - name: offsite\n" +
		"    retention: {daily: 7}\n" +
		"    excludeVolumes: [immich-db-dump]\n" +
		"preHooks:\n" +
		"  - container: immich-postgres\n" +
		"    command: [sh, -c, \"pg_dump\"]\n"
	res, err := Read(map[string]string{"sabon.enable": "true", "sabon.backup": doc}, "sabon", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.HasSpec {
		t.Fatal("HasSpec = false")
	}
	s := res.Spec
	if s.Repo != "immich" {
		t.Errorf("Repo = %q", s.Repo)
	}
	if !reflect.DeepEqual(s.ExtraVolumes, []string{"immich-db-dump"}) {
		t.Errorf("ExtraVolumes = %v", s.ExtraVolumes)
	}
	if len(s.PreHooks) != 1 || s.PreHooks[0].Container != "immich-postgres" {
		t.Errorf("PreHooks = %+v", s.PreHooks)
	}
	if len(s.Targets) != 2 || s.Targets[0].Name != "onsite" || s.Targets[1].Name != "offsite" {
		t.Fatalf("Targets = %+v", s.Targets)
	}
	off, ok := s.TargetOverride("offsite")
	if !ok || off.Retention.Daily != 7 || len(off.ExcludeVolumes) != 1 || off.ExcludeVolumes[0] != "immich-db-dump" {
		t.Errorf("offsite override = %+v (ok=%v)", off, ok)
	}
	if on, _ := s.TargetOverride("onsite"); on.Retention.Daily != 0 || len(on.ExcludeVolumes) != 0 {
		t.Errorf("onsite should carry no overrides: %+v", on)
	}
	if !s.AutoSources() {
		t.Error("AutoSources should default true")
	}
}

func TestReadSpecUnknownFieldFailsClosed(t *testing.T) {
	_, err := Read(map[string]string{"sabon.backup": "repoo: typo\n"}, "sabon", false)
	if err == nil {
		t.Fatal("expected error for unknown field")
	}
}

func TestReadSpecTargetForms(t *testing.T) {
	// bare string form and mapping form parse into the same shape.
	doc := "repo: x\ntargets: [onsite, offsite]\n"
	res, err := Read(map[string]string{"sabon.backup": doc}, "sabon", false)
	if err != nil {
		t.Fatalf("bare targets: %v", err)
	}
	if got := res.Spec.TargetNames(); len(got) != 2 || got[0] != "onsite" || got[1] != "offsite" {
		t.Errorf("TargetNames = %v", got)
	}
}

func TestReadSpecTargetUnknownFieldFailsClosed(t *testing.T) {
	doc := "repo: x\ntargets:\n  - name: onsite\n    excludeVolume: [oops]\n" // typo
	if _, err := Read(map[string]string{"sabon.backup": doc}, "sabon", false); err == nil {
		t.Fatal("expected error for unknown per-target field")
	}
}

func TestReadSpecTargetNameRequired(t *testing.T) {
	doc := "repo: x\ntargets:\n  - schedule: \"0 0 * * * *\"\n" // mapping without name
	if _, err := Read(map[string]string{"sabon.backup": doc}, "sabon", false); err == nil {
		t.Fatal("expected error for target without a name")
	}
}

func TestReadSpecTargetNestedTypoFailsClosed(t *testing.T) {
	doc := "repo: x\ntargets:\n  - name: offsite\n    retention: {dialy: 7}\n" // typo in nested retention
	if _, err := Read(map[string]string{"sabon.backup": doc}, "sabon", false); err == nil {
		t.Fatal("expected error for typo in per-target retention override")
	}
}

func TestReadSpecTargetEmptyNameFailsClosed(t *testing.T) {
	doc := "repo: x\ntargets: [\"\"]\n" // bare empty name
	if _, err := Read(map[string]string{"sabon.backup": doc}, "sabon", false); err == nil {
		t.Fatal("expected error for empty bare target name")
	}
}
