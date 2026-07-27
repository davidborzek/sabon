// Package label reads sabon's container labels off the Docker API and parses
// the document-form backup label into an api.Spec. The label schema itself
// is defined in the api package.
package label

import (
	"bytes"
	"fmt"
	"strconv"

	"github.com/davidborzek/sabon/api"
	"gopkg.in/yaml.v3"
)

// Meta keys (field form) live directly under the prefix.
const (
	keyEnable   = "enable"   // <prefix>.enable: "true"|"false"
	keyInstance = "instance" // <prefix>.instance: "<id>"
	keyBackup   = "backup"   // <prefix>.backup: <document-form YAML Spec>
)

// Result is the outcome of reading one container's labels.
type Result struct {
	Enabled  bool
	HasSpec  bool
	Instance string
	Spec     api.Spec
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
