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
		{"passwordFile ok", []Target{{Name: "x", Path: "/p", PasswordFile: "/etc/sabon/pw"}}, false},
		{"passwordFile relative", []Target{{Name: "x", Path: "/p", PasswordFile: "pw"}}, true},
		{"passwordFile dotdot", []Target{{Name: "x", Path: "/p", PasswordFile: "/etc/link/../pw"}}, true},
		{"passwordFile colon", []Target{{Name: "x", Path: "/p", PasswordFile: "/etc/pw:rw"}}, true},
		{"credentialsFile ok", []Target{{Name: "x", Repo: "s3:y/{app}", CredentialsFile: "/etc/sabon/aws"}}, false},
		{"credentialsFile unclean", []Target{{Name: "x", Repo: "s3:y/{app}", CredentialsFile: "/etc//aws"}}, true},
		{"files ok", []Target{{Name: "x", Repo: "rclone:r:{app}", Files: []TargetFile{{Source: "/etc/sabon/key", Target: "/secrets/key"}, {Source: "/etc/sabon/known_hosts", Target: "/secrets/known_hosts"}}}}, false},
		{"files empty source", []Target{{Name: "x", Path: "/p", Files: []TargetFile{{Target: "/secrets/key"}}}}, true},
		{"files empty target", []Target{{Name: "x", Path: "/p", Files: []TargetFile{{Source: "/k"}}}}, true},
		{"files relative target", []Target{{Name: "x", Path: "/p", Files: []TargetFile{{Source: "/k", Target: "secrets/key"}}}}, true},
		{"files colon in source", []Target{{Name: "x", Path: "/p", Files: []TargetFile{{Source: "/k:rw", Target: "/secrets/key"}}}}, true},
		{"files colon in target", []Target{{Name: "x", Path: "/p", Files: []TargetFile{{Source: "/k", Target: "/secrets/key:rw"}}}}, true},
		{"files duplicate target", []Target{{Name: "x", Path: "/p", Files: []TargetFile{{Source: "/a", Target: "/secrets/key"}, {Source: "/b", Target: "/secrets//key/"}}}}, true},
		{"files reserved exact", []Target{{Name: "x", Path: "/p", Files: []TargetFile{{Source: "/k", Target: "/repo"}}}}, true},
		{"files reserved nested", []Target{{Name: "x", Path: "/p", Files: []TargetFile{{Source: "/k", Target: "/data/app/key"}}}}, true},
		{"files reserved via dotdot", []Target{{Name: "x", Path: "/p", Files: []TargetFile{{Source: "/k", Target: "/secrets/../cache/key"}}}}, true},
		{"files reserved secret", []Target{{Name: "x", Path: "/p", Files: []TargetFile{{Source: "/k", Target: "/run/secrets/sabon-restic-password"}}}}, true},
		{"files reserved root", []Target{{Name: "x", Path: "/p", Files: []TargetFile{{Source: "/k", Target: "/"}}}}, true},
		{"files relative source", []Target{{Name: "x", Path: "/p", Files: []TargetFile{{Source: "relative.key", Target: "/secrets/key"}}}}, true},
		{"files source with dotdot", []Target{{Name: "x", Path: "/p", Files: []TargetFile{{Source: "/etc/link/../key", Target: "/secrets/key"}}}}, true},
		{"files source double slash", []Target{{Name: "x", Path: "/p", Files: []TargetFile{{Source: "/etc//key", Target: "/secrets/key"}}}}, true},
		{"files parent of reserved", []Target{{Name: "x", Path: "/p", Files: []TargetFile{{Source: "/k", Target: "/run/secrets"}}}}, true},
		{"files grandparent of reserved", []Target{{Name: "x", Path: "/p", Files: []TargetFile{{Source: "/k", Target: "/run/"}}}}, true},
		{"files sibling under /run ok", []Target{{Name: "x", Path: "/p", Files: []TargetFile{{Source: "/k", Target: "/run/secrets/rclone-key"}}}}, false},
		{"files prefix lookalike ok", []Target{{Name: "x", Path: "/p", Files: []TargetFile{{Source: "/k", Target: "/database/key"}}}}, false},
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
