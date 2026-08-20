package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/urfave/cli/v3"
)

func TestHealthURL(t *testing.T) {
	cases := []struct {
		addr, endpoint, want string
	}{
		{":9333", "readyz", "http://127.0.0.1:9333/readyz"},
		{"0.0.0.0:9333", "healthz", "http://127.0.0.1:9333/healthz"},
		{"127.0.0.1:8080", "/readyz", "http://127.0.0.1:8080/readyz"},
		{"[::]:9333", "readyz", "http://[::1]:9333/readyz"},
		{"[::1]:9333", "readyz", "http://[::1]:9333/readyz"},
	}
	for _, c := range cases {
		got, err := healthURL(c.addr, c.endpoint)
		if err != nil || got != c.want {
			t.Errorf("healthURL(%q, %q) = %q, %v; want %q", c.addr, c.endpoint, got, err, c.want)
		}
	}

	// An unknown endpoint is a usage error, not a silent probe of a 404.
	if _, err := healthURL(":9333", "metrics"); err == nil {
		t.Error("unknown endpoint must be rejected")
	}
	// A port-less address cannot be probed.
	if _, err := healthURL("localhost", "readyz"); err == nil {
		t.Error("address without a port must be rejected")
	}
}

// healthcheckCmd builds the healthcheck command with the flags main.go declares
// so tests exercise the same defaults the CLI does.
func healthcheckCmd() *cli.Command {
	return &cli.Command{
		Name:   "healthcheck",
		Action: runHealthcheck,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "endpoint", Value: "readyz"},
			&cli.StringFlag{Name: "addr"},
			&cli.DurationFlag{Name: "timeout", Value: healthcheckTimeout},
		},
	}
}

func runHealthcheckArgs(t *testing.T, args ...string) error {
	t.Helper()
	return healthcheckCmd().Run(context.Background(), append([]string{"healthcheck"}, args...))
}

func TestRunHealthcheck(t *testing.T) {
	var status int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" {
			t.Errorf("probed %q, want /readyz", r.URL.Path)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte("not ready"))
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	status = http.StatusOK
	if err := runHealthcheckArgs(t, "--addr", addr); err != nil {
		t.Errorf("healthy endpoint = %v, want nil", err)
	}

	// 503 (sabon not ready yet) must surface as an error so Docker marks the
	// container unhealthy.
	status = http.StatusServiceUnavailable
	if err := runHealthcheckArgs(t, "--addr", addr); err == nil {
		t.Error("503 must fail the healthcheck")
	}
}

func TestRunHealthcheckUnreachable(t *testing.T) {
	// A closed port: nothing listening means unhealthy, not a hang.
	srv := httptest.NewServer(http.NotFoundHandler())
	addr := strings.TrimPrefix(srv.URL, "http://")
	srv.Close()

	if err := runHealthcheckArgs(t, "--addr", addr, "--timeout", (2 * time.Second).String()); err == nil {
		t.Error("unreachable endpoint must fail the healthcheck")
	}
}

func TestRunHealthcheckEndpointsDisabled(t *testing.T) {
	// Nothing serves the endpoints, so there is nothing to probe: stay healthy
	// instead of failing a container that opted out on purpose.
	t.Setenv("SABON_METRICS_ADDR", "")
	t.Setenv("SABON_HEALTH_ADDR", "")
	if err := runHealthcheckArgs(t); err != nil {
		t.Errorf("disabled health endpoints = %v, want nil", err)
	}
}

func TestRunHealthcheckProbesHealthListener(t *testing.T) {
	// With metrics off, the probe must find the loopback health listener.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	t.Setenv("SABON_METRICS_ADDR", "")
	t.Setenv("SABON_HEALTH_ADDR", strings.TrimPrefix(srv.URL, "http://"))
	if err := runHealthcheckArgs(t); err != nil {
		t.Errorf("health listener probe = %v, want nil", err)
	}
}
