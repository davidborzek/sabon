package api

import "testing"

func TestDestinationMarkerValidation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		edit  func(*Target)
		valid bool
	}{
		{"local", func(t *Target) {}, true},
		{"remote", func(t *Target) { t.Path = ""; t.Repo = "sftp:host:/backup" }, false},
		{"whitespace", func(t *Target) { t.DestinationMarker = " disk " }, false},
		{"repository-env", func(t *Target) { t.Env = map[string]string{"RESTIC_REPOSITORY": "/other"} }, false},
		{"repository-file-env", func(t *Target) { t.Env = map[string]string{"RESTIC_REPOSITORY_FILE": "/file"} }, false},
		{"repo", func(t *Target) { t.ResticArgs = []string{"--repo=/other"} }, false},
		{"repo-file", func(t *Target) { t.ResticArgs = []string{"--repository-file", "/file"} }, false},
		{"short", func(t *Target) { t.ResticArgs = []string{"-r", "/other"} }, false},
		{"attached", func(t *Target) { t.ResticArgs = []string{"-r/other"} }, false},
		{"cluster", func(t *Target) { t.ResticArgs = []string{"-qr", "/other"} }, false},
		{"attached-cluster", func(t *Target) { t.ResticArgs = []string{"-qr/other"} }, false},
		{"normal", func(t *Target) { t.ResticArgs = []string{"-q", "-v", "-o", "s3.connections=2", "--retry-lock=1m"} }, true},
		{"unguarded", func(t *Target) { t.DestinationMarker = ""; t.ResticArgs = []string{"-qr/other"} }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := Target{Name: "backup", Path: "/backup", DestinationMarker: "disk"}
			tc.edit(&target)
			f := File{Targets: []Target{target}}
			if err := f.Validate(); (err == nil) != tc.valid {
				t.Fatalf("Validate() = %v, want valid=%v", err, tc.valid)
			}
		})
	}
}
