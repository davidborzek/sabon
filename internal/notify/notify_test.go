package notify

import (
	"strings"
	"testing"
	"time"
)

func TestNewDisabledNoop(t *testing.T) {
	n, err := New(nil, "", "")
	if err != nil {
		t.Fatalf("New(nil) error: %v", err)
	}
	if n.Enabled() {
		t.Fatal("no URLs should be disabled")
	}
	if err := n.Send(Data{Event: "backup"}); err != nil {
		t.Fatalf("Send on disabled notifier should be a no-op: %v", err)
	}
}

func TestNewInvalidURL(t *testing.T) {
	if _, err := New([]string{"not-a-valid-url"}, "", ""); err == nil {
		t.Fatal("expected error for invalid shoutrrr URL")
	}
}

func TestNewInvalidTemplateFailsClosed(t *testing.T) {
	if _, err := New([]string{"logger://"}, "{{.Oops", ""); err == nil {
		t.Fatal("expected error for unparseable title template")
	}
	if _, err := New([]string{"logger://"}, "", "{{end}}"); err == nil {
		t.Fatal("expected error for unparseable body template")
	}
}

func TestMultipleURLs(t *testing.T) {
	n, err := New([]string{"logger://", "logger://"}, "", "")
	if err != nil {
		t.Fatalf("New(two urls): %v", err)
	}
	if !n.Enabled() {
		t.Fatal("two URLs should be enabled")
	}
	if err := n.Send(Data{Event: "backup", App: "immich", Target: "onsite", OK: true}); err != nil {
		t.Fatalf("Send to logger sinks: %v", err)
	}
}

func TestDefaultTemplatesRender(t *testing.T) {
	n, err := New([]string{"logger://"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	ok := Data{Event: "backup", App: "immich", Target: "onsite", OK: true,
		Duration: 90 * time.Second, SnapshotID: "abcdef1234567", FilesNew: 5, DataAdded: 3 * 1024 * 1024}
	// Exact output locks the embedded templates' whitespace trimming.
	if got := render(n.title, ok, "x"); got != "sabon: immich → onsite ok" {
		t.Errorf("backup-ok title = %q", got)
	}
	if got := render(n.body, ok, "x"); got != "Backup of immich to onsite succeeded in 1m30s (snapshot abcdef12, 5 new files, 3.0 MiB)." {
		t.Errorf("backup-ok body = %q", got)
	}

	fail := Data{Event: "backup", App: "immich", Target: "offsite", Duration: time.Second, Error: "boom"}
	if title := render(n.title, fail, "x"); !strings.Contains(title, "FAILED") {
		t.Errorf("backup-fail title = %q", title)
	}
	if body := render(n.body, fail, "x"); !strings.Contains(body, "boom") {
		t.Errorf("backup-fail body = %q", body)
	}

	chk := Data{Event: "check", App: "immich", Target: "onsite", Error: "corrupt"}
	if title := render(n.title, chk, "x"); !strings.Contains(title, "check FAILED") {
		t.Errorf("check title = %q", title)
	}
	if body := render(n.body, chk, "x"); !strings.Contains(body, "restic check") {
		t.Errorf("check body = %q", body)
	}
}

func TestCustomTemplatesRender(t *testing.T) {
	n, err := New([]string{"logger://"}, "T:{{.App}}", "B:{{.Event}}/{{.OK}}/{{.Instance}}")
	if err != nil {
		t.Fatal(err)
	}
	d := Data{Event: "backup", App: "immich", Instance: "prod", OK: true}
	if got := render(n.title, d, "x"); got != "T:immich" {
		t.Errorf("custom title = %q", got)
	}
	if got := render(n.body, d, "x"); got != "B:backup/true/prod" {
		t.Errorf("custom body = %q", got)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[uint64]string{0: "0 B", 512: "512 B", 1024: "1.0 KiB", 3 * 1024 * 1024: "3.0 MiB"}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}
