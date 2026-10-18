//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSFTPTargets backs up to an SFTP server through both ways the mover image
// supports: restic's native sftp: backend (execs ssh) and rclone: (rclone's own
// SFTP client). Key and known_hosts reach the mover via the target's `files`,
// and a wrong host key must fail the run. Run with:
//
//	go test -tags e2e -run SFTP ./test/e2e/
func TestSFTPTargets(t *testing.T) {
	if _, err := run(t, "docker", "version"); err != nil {
		t.Skip("docker not available")
	}
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := run(t, "docker", "build", "-f", filepath.Join(repoRoot, "Dockerfile.mover"), "-t", moverImage, "--build-arg", "VERSION=e2e", repoRoot); err != nil {
		t.Fatalf("docker build: %v\n%s", err, out)
	}
	bin := filepath.Join(t.TempDir(), "sabon")
	if out, err := run(t, "go", "build", "-o", bin, filepath.Join(repoRoot, "cmd", "sabon")); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	// Not t.TempDir: keys are generated and the server writes as root.
	work, err := os.MkdirTemp("", "sabon-e2e-sftp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = run(t, "docker", "run", "--rm", "-v", work+":/w", "alpine:3", "sh", "-c", "rm -rf /w/keys /w/srv")
		_ = os.RemoveAll(work)
	})
	keys := filepath.Join(work, "keys")
	srv := filepath.Join(work, "srv")
	src := filepath.Join(work, "src")
	for _, d := range []string{keys, srv, src} {
		if err := os.MkdirAll(d, 0o777); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(src, "data.txt"), []byte("sftp payload "+time.Now().Format(time.RFC3339Nano)), 0o644); err != nil {
		t.Fatal(err)
	}

	// Client and host keys, generated with the mover image's own ssh-keygen.
	for _, k := range []string{"client", "host"} {
		mustRun(t, nil, "docker", "run", "--rm", "-v", keys+":/k", "--entrypoint", "ssh-keygen", moverImage, "-q", "-t", "ed25519", "-N", "", "-f", "/k/"+k)
	}
	hostPub, err := os.ReadFile(filepath.Join(keys, "host.pub"))
	if err != nil {
		t.Fatal(err)
	}
	clientPub, err := os.ReadFile(filepath.Join(keys, "client.pub"))
	if err != nil {
		t.Fatal(err)
	}
	knownHosts := filepath.Join(work, "known_hosts")
	badKnownHosts := filepath.Join(work, "bad_known_hosts")
	if err := os.WriteFile(knownHosts, []byte("[sftp]:2022 "+keyField(hostPub)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(badKnownHosts, []byte("[sftp]:2022 "+keyField(clientPub)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// SFTP server on a user-defined network so movers reach it by name.
	network := "sabon-e2e-sftp"
	server := "sabon-e2e-sftp-server"
	_, _ = run(t, "docker", "rm", "-f", server)
	_, _ = run(t, "docker", "network", "rm", network)
	mustRun(t, nil, "docker", "network", "create", network)
	t.Cleanup(func() { _, _ = run(t, "docker", "network", "rm", network) })
	mustRun(t, nil, "docker", "run", "-d", "--name", server, "--network", network, "--network-alias", "sftp",
		"-v", keys+":/k:ro", "-v", srv+":/srv",
		"rclone/rclone:1.75.2", "serve", "sftp", "/srv", "--addr", ":2022", "--key", "/k/host", "--authorized-keys", "/k/client.pub")
	t.Cleanup(func() { _, _ = run(t, "docker", "rm", "-f", server) })
	waitSFTP(t, network)

	clientKey := filepath.Join(keys, "client")
	sshArgs := "-p 2022 -i /secrets/key -o UserKnownHostsFile=/secrets/known_hosts -o StrictHostKeyChecking=yes -o BatchMode=yes"
	targets := filepath.Join(work, "targets.yaml")
	writeTargets := func(rcloneKnownHosts string) {
		t.Helper()
		yaml := fmt.Sprintf(`targets:
  - name: native
    repo: "sftp:e2e@sftp:/native/{app}"
    files:
      - {source: %[1]s, target: /secrets/key}
      - {source: %[2]s, target: /secrets/known_hosts}
    resticArgs: ["-o", "sftp.args=%[3]s"]
  - name: rclone
    repo: "rclone:box:rclone/{app}"
    files:
      - {source: %[1]s, target: /secrets/key}
      - {source: %[4]s, target: /secrets/known_hosts}
    env:
      RCLONE_CONFIG_BOX_TYPE: sftp
      RCLONE_CONFIG_BOX_HOST: sftp
      RCLONE_CONFIG_BOX_PORT: "2022"
      RCLONE_CONFIG_BOX_USER: e2e
      RCLONE_CONFIG_BOX_KEY_FILE: /secrets/key
      RCLONE_CONFIG_BOX_KNOWN_HOSTS_FILE: /secrets/known_hosts
`, clientKey, knownHosts, sshArgs, rcloneKnownHosts)
		if err := os.WriteFile(targets, []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeTargets(knownHosts)

	sftpApp := "e2esftp"
	container := "sabon-e2e-sftp-app"
	_, _ = run(t, "docker", "rm", "-f", container)
	spec := "repo: " + sftpApp + "\nauto: false\nextraPaths: [" + src + "]\n"
	mustRun(t, nil, "docker", "run", "-d", "--name", container,
		"-v", src+":/data",
		"--label", "sabon.enable=true",
		"--label", "sabon.backup="+spec,
		"alpine:3", "sleep", "600")
	t.Cleanup(func() { _, _ = run(t, "docker", "rm", "-f", container) })

	env := []string{
		"SABON_CONFIG=" + targets,
		"SABON_MOVER_IMAGE=" + moverImage,
		"SABON_MOVER_NETWORK=" + network,
		"RESTIC_PASSWORD=" + password,
	}

	out := mustRun(t, env, bin, "backup")
	if !strings.Contains(out, "backup complete") {
		t.Fatalf("backup did not complete:\n%s", out)
	}
	for _, target := range []string{"native", "rclone"} {
		out = mustRun(t, env, bin, "snapshots", "--app", sftpApp, "--target", target)
		if !strings.Contains(out, sftpApp) {
			t.Fatalf("%s: snapshots did not list the app:\n%s", target, out)
		}
		if _, err := os.Stat(filepath.Join(srv, target, sftpApp, "config")); err != nil {
			t.Errorf("%s: no restic repo on the SFTP server: %v", target, err)
		}
	}

	// A host key that doesn't match must fail instead of trusting the server.
	writeTargets(badKnownHosts)
	cmdOut, err := runEnv(env, bin, "snapshots", "--app", sftpApp, "--target", "rclone")
	if err == nil {
		t.Fatalf("snapshots with a wrong host key succeeded:\n%s", cmdOut)
	}
	if !strings.Contains(cmdOut, "key mismatch") {
		t.Errorf("wrong host key: want a key mismatch error, got:\n%s", cmdOut)
	}
}

// keyField returns "<type> <base64>" from an OpenSSH public key line.
func keyField(pub []byte) string {
	f := strings.Fields(string(pub))
	if len(f) < 2 {
		return ""
	}
	return f[0] + " " + f[1]
}

// waitSFTP blocks until the SFTP server accepts TCP connections on the network.
func waitSFTP(t *testing.T, network string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := run(t, "docker", "run", "--rm", "--network", network, "--entrypoint", "nc", moverImage, "-z", "-w", "2", "sftp", "2022"); err == nil {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatal("SFTP server did not come up")
}
