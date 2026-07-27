---
name: Bug report
about: Report a problem with sabon
labels: bug
---

**What happened**

A clear description of the bug.

**Expected behaviour**

What you expected to happen instead.

**Configuration**

- Relevant `SABON_*` variables (restic repository, backend, schedule, label
  prefix) — redact passwords and backend credentials:
- The container backup labels that triggered the behaviour:
- `/metrics` output if relevant (e.g. backup success/failure counters):

**Logs**

Run with `SABON_LOG_LEVEL=debug` and paste the relevant output (redact secrets).

**Environment**

- sabon image tag / version:
- Docker version:
- restic version:
