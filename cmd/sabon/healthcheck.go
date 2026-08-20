package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/davidborzek/sabon/internal/config"
	"github.com/urfave/cli/v3"
)

// healthcheckTimeout stays well under the HEALTHCHECK --timeout the image declares.
const healthcheckTimeout = 3 * time.Second

// runHealthcheck probes sabon's own health endpoint over loopback and exits
// non-zero when it is not healthy. It exists because the image is distroless:
// there is no shell, curl or wget for a HEALTHCHECK to use.
func runHealthcheck(ctx context.Context, cmd *cli.Command) error {
	addr := cmd.String("addr")
	if addr == "" {
		addr = config.HealthProbeAddr()
	}
	if addr == "" {
		fmt.Println("ok (health endpoints disabled, nothing to probe)")
		return nil
	}

	url, err := healthURL(addr, cmd.String("endpoint"))
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, cmd.Duration("timeout"))
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	// Proxy: nil — an inherited HTTP_PROXY would send this loopback probe off-host.
	client := &http.Client{Transport: &http.Transport{Proxy: nil}}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("probe %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("probe %s: %s: %s", url, resp.Status, strings.TrimSpace(string(body)))
	}
	fmt.Println("ok")
	return nil
}

func healthURL(addr, endpoint string) (string, error) {
	endpoint = strings.TrimPrefix(endpoint, "/")
	if endpoint != "healthz" && endpoint != "readyz" {
		return "", fmt.Errorf("--endpoint must be \"readyz\" or \"healthz\", got %q", endpoint)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("invalid listen address %q: %w", addr, err)
	}
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::":
		host = "::1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/" + endpoint, nil
}
