// Package discovery defines the shared backup-job domain types (Job, Source)
// and the helpers that resolve, name, and validate backup sources. The concrete
// discoverers live with their runtime — engine/docker lists sabon-labelled
// containers, engine/swarm lists sabon-labelled services — and both produce the
// Job type defined here.
package discovery

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/davidborzek/sabon/api"
	"github.com/docker/docker/api/types/mount"
)

// Compose labels sabon reads to derive the app/project when the spec omits repo.
const (
	ComposeServiceLabel = "com.docker.compose.service"
	ComposeProjectLabel = "com.docker.compose.project"
)

// Source is one thing to back up, mounted read-only into the mover at
// /data/<Name>.
type Source struct {
	Name string     // stable, unique mount name (the /data/<Name> segment)
	Type mount.Type // bind or volume
	Ref  string     // host path (bind) or volume name (volume)
}

// Job is a resolved backup unit for one app.
type Job struct {
	Container string   // labelled container name (standalone) or service ref (swarm)
	App       string   // repository/app name
	Instance  string   // owning sabon instance (from <prefix>.instance)
	Project   string   // labelled container's compose project (scopes exec hooks)
	Spec      api.Spec // parsed backup spec
	Sources   []Source // concrete sources
	Node      string   // swarm: hostname of the node the task runs on (empty on standalone)
}

// ValidAppName rejects app/repo names that could escape a target's base path
// once joined into it (or into a remote {app} template).
func ValidAppName(app string) error {
	if app == "" {
		return fmt.Errorf("empty app/repo name")
	}
	if strings.ContainsAny(app, `/\`) || strings.Contains(app, "..") {
		return fmt.Errorf("app/repo name %q must not contain path separators or %q", app, "..")
	}
	return nil
}

// Signal does a non-blocking send on a coalescing (buffered, cap 1) channel, so
// a burst of events collapses into a single wakeup.
func Signal(ch chan<- struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

var unsafeName = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// BaseName derives a readable mount name, preferring the destination path.
func BaseName(dest, src string) string {
	b := path.Base(strings.TrimRight(dest, "/"))
	if b == "" || b == "/" || b == "." {
		b = path.Base(strings.TrimRight(src, "/"))
	}
	b = unsafeName.ReplaceAllString(b, "-")
	b = strings.Trim(b, "-._")
	if b == "" {
		b = "data"
	}
	return b
}

// Namer hands out unique, filesystem-safe names, suffixing collisions and
// reserving every name it returns so a generated suffix cannot later collide.
type Namer struct{ seen map[string]bool }

// NewNamer returns an empty Namer.
func NewNamer() *Namer { return &Namer{seen: map[string]bool{}} }

// Pick returns a filesystem-safe, unique name derived from base.
func (n *Namer) Pick(base string) string {
	base = unsafeName.ReplaceAllString(base, "-")
	if base == "" {
		base = "data"
	}
	name := base
	for i := 1; n.seen[name]; i++ {
		name = base + "-" + itoa(i)
	}
	n.seen[name] = true
	return name
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	return string(b[p:])
}
