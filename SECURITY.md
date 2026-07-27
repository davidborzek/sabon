# Security policy

## Reporting a vulnerability

Please **do not** open a public issue for security vulnerabilities.

Instead, report them privately via GitHub's
[security advisories](https://github.com/davidborzek/sabon/security/advisories/new)
("Report a vulnerability"). You will receive a response as soon as possible, and
disclosure will be coordinated with you.

## Scope

sabon requires **write** access to the Docker API: to back up a container's
volumes it spawns ephemeral "mover" containers and mounts arbitrary volumes into
them. This is a highly privileged capability — anyone able to reach sabon (or its
Docker socket) can effectively run containers with arbitrary mounts on the host.
Run sabon on a trusted single host, restrict access to the Docker socket to the
minimum required, and do not expose its control surface to untrusted networks.

sabon also handles secrets: restic repository passwords and backend credentials
(e.g. S3/R2 access keys) are provided via environment variables. Keep these out
of version control, inject them from a secret store or environment at runtime,
and treat the restic repository and its backend as sensitive.
