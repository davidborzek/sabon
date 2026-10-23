// Package label reads sabon's container labels off the Docker API and parses
// the document-form backup label into an api.Spec. The label schema itself
// is defined in the api package.
package label

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/davidborzek/sabon/api"
	"gopkg.in/yaml.v3"
)

// Meta keys (field form) live directly under the prefix.
const (
	keyEnable   = "enable"   // <prefix>.enable: "true"|"false"
	keyInstance = "instance" // <prefix>.instance: "<id>"
	keyBackup   = "backup"   // <prefix>.backup[.<name>]: <document-form YAML Spec>
)

// validName is the charset of a <name> in <prefix>.backup.<name>: one
// lower-case Docker-label segment, so the key stays unambiguous.
var validName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// Result is the outcome of reading one container's labels.
type Result struct {
	Enabled  bool
	HasSpec  bool // the singleton <prefix>.backup label is present
	Instance string
	Spec     api.Spec // the singleton spec
	Named    []Named  // <prefix>.backup.<name> specs, sorted by name
}

// Named is one <prefix>.backup.<name> spec: an independent backup on the same
// container, with its own repository.
type Named struct {
	Name string
	Spec api.Spec
}

// HasBackups reports whether the container carries any backup spec.
func (r Result) HasBackups() bool { return r.HasSpec || len(r.Named) > 0 }

// App is the backup's repository name: the spec's repo, else serviceName for
// the singleton and "<serviceName>-<name>" for a named backup, so the same name
// on two services does not collide.
func (n Named) App(serviceName string) string {
	switch {
	case n.Spec.Repo != "":
		return n.Spec.Repo
	case n.Name != "":
		return serviceName + "-" + n.Name
	default:
		return serviceName
	}
}

// Backups lists every backup spec: the singleton first (Name ""), then the
// named ones.
func (r Result) Backups() []Named {
	var out []Named
	if r.HasSpec {
		out = append(out, Named{Spec: r.Spec})
	}
	return append(out, r.Named...)
}

// Read parses sabon's labels off a container's label map. watchByDefault flips
// the enable semantics: on -> a container is included unless <prefix>.enable is
// "false"; off -> only when <prefix>.enable is "true".
func Read(labels map[string]string, prefix string, watchByDefault bool) (Result, error) {
	var res Result

	enableRaw, hasEnable := labels[prefix+"."+keyEnable]
	if watchByDefault {
		res.Enabled = true
	}
	if hasEnable {
		b, err := strconv.ParseBool(enableRaw)
		if err != nil {
			return res, fmt.Errorf("label %s.%s: %w", prefix, keyEnable, err)
		}
		res.Enabled = b
	}

	res.Instance = labels[prefix+"."+keyInstance]

	if doc, ok := labels[prefix+"."+keyBackup]; ok {
		spec, err := parseSpec(doc)
		if err != nil {
			return res, fmt.Errorf("label %s.%s: %w", prefix, keyBackup, err)
		}
		res.Spec = spec
		res.HasSpec = true
	}

	namedPrefix := prefix + "." + keyBackup + "."
	for key, doc := range labels {
		name, ok := strings.CutPrefix(key, namedPrefix)
		if !ok {
			continue
		}
		if !validName.MatchString(name) {
			return res, fmt.Errorf("label %s: backup name %q must match %s", key, name, validName)
		}
		spec, err := parseSpec(doc)
		if err != nil {
			return res, fmt.Errorf("label %s: %w", key, err)
		}
		res.Named = append(res.Named, Named{Name: name, Spec: spec})
	}
	sort.Slice(res.Named, func(i, j int) bool { return res.Named[i].Name < res.Named[j].Name })

	// A bare <prefix>.enable with no spec still counts as opt-in intent, but
	// there is nothing to back up; callers treat HasSpec as the real gate.
	return res, nil
}

// parseSpec decodes a document-form Spec with strict field checking so typos
// fail closed instead of being silently ignored.
func parseSpec(doc string) (api.Spec, error) {
	dec := yaml.NewDecoder(bytes.NewReader([]byte(doc)))
	dec.KnownFields(true)
	var spec api.Spec
	if err := dec.Decode(&spec); err != nil {
		return api.Spec{}, err
	}
	return spec, nil
}
