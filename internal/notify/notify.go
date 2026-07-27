// Package notify sends backup notifications via shoutrrr to one or more service
// URLs, so sabon can alert on outcomes without a Prometheus/Alertmanager stack.
// It is optional: no URLs yields a nil Notifier whose Send is a no-op. Title and
// body are rendered from Go text/templates (overridable) so the wording and
// fields can be tailored per sink.
package notify

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"text/template"
	"time"

	"github.com/containrrr/shoutrrr"
	"github.com/containrrr/shoutrrr/pkg/router"
	"github.com/containrrr/shoutrrr/pkg/types"
)

// Data is the template context for one notification.
type Data struct {
	Event      string        // "backup" | "check" | "prune"
	App        string        // app / repo name
	Target     string        // target name
	Instance   string        // owning sabon instance (SABON_INSTANCE), if set
	OK         bool          // outcome
	Duration   time.Duration // how long the operation took
	SnapshotID string        // created snapshot (backup success)
	FilesNew   int           // new files (backup success)
	DataAdded  uint64        // bytes added to the repo (backup success)
	Error      string        // failure cause ("" on success)
}

// Default templates reproduce sabon's built-in wording; both are overridable via
// SABON_NOTIFY_TITLE_TEMPLATE / SABON_NOTIFY_TEMPLATE.
//
//go:embed templates/title.tmpl
var DefaultTitleTemplate string

//go:embed templates/body.tmpl
var DefaultBodyTemplate string

var funcs = template.FuncMap{
	"short": func(s string) string {
		if len(s) > 8 {
			return s[:8]
		}
		return s
	},
	"bytes": humanBytes,
}

// Notifier delivers rendered messages to one or more shoutrrr services.
type Notifier struct {
	sr    *router.ServiceRouter
	title *template.Template
	body  *template.Template
}

// New returns a Notifier for the given shoutrrr URLs. No URLs returns a nil
// Notifier (disabled). Empty template strings fall back to the defaults. An
// invalid URL or unparseable template returns an error, so misconfiguration
// fails closed at startup rather than at the first alert.
func New(urls []string, titleTmpl, bodyTmpl string) (*Notifier, error) {
	if len(urls) == 0 {
		return nil, nil
	}
	sr, err := shoutrrr.CreateSender(urls...)
	if err != nil {
		return nil, fmt.Errorf("notify: %w", err)
	}
	if titleTmpl == "" {
		titleTmpl = DefaultTitleTemplate
	}
	if bodyTmpl == "" {
		bodyTmpl = DefaultBodyTemplate
	}
	title, err := template.New("title").Funcs(funcs).Parse(titleTmpl)
	if err != nil {
		return nil, fmt.Errorf("notify: SABON_NOTIFY_TITLE_TEMPLATE: %w", err)
	}
	body, err := template.New("body").Funcs(funcs).Parse(bodyTmpl)
	if err != nil {
		return nil, fmt.Errorf("notify: SABON_NOTIFY_TEMPLATE: %w", err)
	}
	return &Notifier{sr: sr, title: title, body: body}, nil
}

// Enabled reports whether notifications are configured.
func (n *Notifier) Enabled() bool { return n != nil && n.sr != nil }

// Send renders the title and body from data and delivers to every service. A
// nil or unconfigured Notifier is a no-op. Per-service delivery errors are
// joined; a template render error falls back to a plain message so an alert is
// never silently dropped.
func (n *Notifier) Send(data Data) error {
	if !n.Enabled() {
		return nil
	}
	title := render(n.title, data, fmt.Sprintf("sabon: %s → %s", data.App, data.Target))
	body := render(n.body, data, fmt.Sprintf("sabon %s of %s → %s: ok=%v %s", data.Event, data.App, data.Target, data.OK, data.Error))
	params := types.Params{"title": title}
	var errs []error
	for _, err := range n.sr.Send(body, &params) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func render(t *template.Template, data Data, fallback string) string {
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return fallback
	}
	return buf.String()
}

// humanBytes renders a byte count as a binary-prefixed size (e.g. "12.0 MiB").
func humanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
