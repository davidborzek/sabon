package api

import "testing"

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		targets []Target
		wantErr bool
	}{
		{"valid local + remote", []Target{{Name: "onsite", Path: "/mnt/backup"}, {Name: "offsite", Repo: "s3:x/{app}"}}, false},
		{"missing name", []Target{{Path: "/mnt/backup"}}, true},
		{"both path and repo", []Target{{Name: "x", Path: "/p", Repo: "s3:y"}}, true},
		{"neither path nor repo", []Target{{Name: "x"}}, true},
		{"duplicate name", []Target{{Name: "x", Path: "/p"}, {Name: "x", Repo: "s3:y"}}, true},
		{"moverLabels ok", []Target{{Name: "x", Path: "/p", MoverLabels: map[string]string{"tier": "onsite"}}}, false},
		{"moverLabels reserved prefix", []Target{{Name: "x", Path: "/p", MoverLabels: map[string]string{"sabon.app": "evil"}}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &File{Targets: tt.targets}
			err := f.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestBackend(t *testing.T) {
	if got := (Target{Path: "/mnt/backup"}).Backend(); got != BackendLocal {
		t.Errorf("local Backend = %q", got)
	}
	if got := (Target{Repo: "s3:x/{app}"}).Backend(); got != BackendRemote {
		t.Errorf("remote Backend = %q", got)
	}
}
