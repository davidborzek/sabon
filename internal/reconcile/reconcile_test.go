package reconcile

import (
	"strings"
	"testing"

	"github.com/davidborzek/sabon/api"
	"github.com/davidborzek/sabon/internal/discovery"
)

func plansFor(pairs ...string) []plan {
	var out []plan
	for _, p := range pairs {
		app, target, _ := strings.Cut(p, "|")
		out = append(out, plan{job: discovery.Job{App: app}, target: api.Target{Name: target}})
	}
	return out
}

func names(ps []plan) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.job.App+"|"+p.target.Name)
	}
	return out
}

func TestFilterPlans(t *testing.T) {
	all := plansFor("a|onsite", "a|offsite", "b|onsite", "c|offsite")
	cases := []struct {
		app, target string
		want        string
	}{
		{"", "", "a|onsite a|offsite b|onsite c|offsite"},
		{"a", "", "a|onsite a|offsite"},
		{"", "onsite", "a|onsite b|onsite"},
		{"a", "offsite", "a|offsite"},
	}
	for _, c := range cases {
		got, err := filterPlans(all, c.app, c.target)
		if err != nil {
			t.Errorf("app=%q target=%q: unexpected error %v", c.app, c.target, err)
			continue
		}
		if g := strings.Join(names(got), " "); g != c.want {
			t.Errorf("app=%q target=%q: got %q, want %q", c.app, c.target, g, c.want)
		}
	}
}

// A filter that selects nothing is almost always a typo; a silent no-op would
// look like a successful backup.
func TestFilterPlansNoMatchFails(t *testing.T) {
	all := plansFor("a|onsite", "b|offsite")
	for _, c := range []struct{ app, target, wantErr string }{
		{"nope", "", `no backup job for app "nope"`},
		{"", "nope", `no app backs up to target "nope"`},
		{"a", "offsite", `app "a" does not back up to target "offsite"`},
	} {
		_, err := filterPlans(all, c.app, c.target)
		if err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("app=%q target=%q: err = %v, want %q", c.app, c.target, err, c.wantErr)
		}
	}
}
