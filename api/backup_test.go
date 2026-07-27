package api

import (
	"reflect"
	"testing"
)

func TestRetention(t *testing.T) {
	if !(Retention{}).Empty() {
		t.Error("zero Retention should be Empty")
	}
	r := Retention{Hourly: 24, Daily: 7, Within: "30d"}
	if r.Empty() {
		t.Error("non-zero Retention should not be Empty")
	}
	got := r.ForgetArgs()
	want := []string{"--keep-hourly", "24", "--keep-daily", "7", "--keep-within", "30d"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ForgetArgs = %v, want %v", got, want)
	}
	if (Retention{}).ForgetArgs() != nil {
		t.Error("empty Retention should yield nil args")
	}
}

func TestHookModes(t *testing.T) {
	exec := Hook{Command: []string{"true"}}
	if exec.Mode() != "exec" {
		t.Errorf("Mode = %q, want exec", exec.Mode())
	}
	run := Hook{Image: "postgres:16", Command: []string{"pg_dump"}}
	if run.Mode() != "run" {
		t.Errorf("Mode = %q, want run", run.Mode())
	}
}

func TestHookTimeout(t *testing.T) {
	d, err := (Hook{Timeout: "5m"}).TimeoutDuration()
	if err != nil || d.Minutes() != 5 {
		t.Errorf("TimeoutDuration = %v, %v", d, err)
	}
	if d, _ := (Hook{}).TimeoutDuration(); d != 0 {
		t.Errorf("empty timeout should be 0, got %v", d)
	}
	if _, err := (Hook{Timeout: "nope"}).TimeoutDuration(); err == nil {
		t.Error("expected error for invalid duration")
	}
}

func TestSpecBackupTimeout(t *testing.T) {
	d, err := (Spec{Timeout: "2h"}).BackupTimeout()
	if err != nil || d.Hours() != 2 {
		t.Errorf("BackupTimeout = %v, %v", d, err)
	}
	if d, _ := (Spec{}).BackupTimeout(); d != 0 {
		t.Errorf("empty timeout should be 0, got %v", d)
	}
	if _, err := (Spec{Timeout: "nope"}).BackupTimeout(); err == nil {
		t.Error("expected error for invalid duration")
	}
}

func TestSpecMoverUser(t *testing.T) {
	if got := (Spec{User: "568:568"}).MoverUser("0:0"); got != "568:568" {
		t.Errorf("label override: got %q, want 568:568", got)
	}
	if got := (Spec{}).MoverUser("0:0"); got != "0:0" {
		t.Errorf("fallback: got %q, want 0:0", got)
	}
}

func TestSpecMoverGroups(t *testing.T) {
	if got := (Spec{Groups: []string{"4242"}}).MoverGroups([]string{"1"}); len(got) != 1 || got[0] != "4242" {
		t.Errorf("label override: got %v, want [4242]", got)
	}
	if got := (Spec{}).MoverGroups([]string{"1"}); len(got) != 1 || got[0] != "1" {
		t.Errorf("fallback: got %v, want [1]", got)
	}
}
