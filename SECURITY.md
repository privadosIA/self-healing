# Security Policy

## Supported versions

This project is a proof of concept. Only the latest commit on `main` receives security fixes.

## Reporting a vulnerability

Please do **not** open a public issue for a security problem.

Report it privately through GitHub's private vulnerability reporting:
<https://github.com/privadosIA/self-healing/security/advisories/new>

Include:

- a description of the problem and its impact,
- the steps or a minimal payload to reproduce it (use synthetic data only),
- the affected commit or version.

You can expect an acknowledgement within 7 days. Fixes are coordinated with the reporter, and the advisory is published once a fix is available.

## Scope

In scope: the `healerd` daemon, its HTTP endpoints (webhook receiver, report upload, health and metrics), the handling of secrets, and the patch validation and pull request logic.

Out of scope: vulnerabilities in Gitea, SonarQube, Trivy or the LLM server themselves, and misconfigurations of a deployment (for example exposing `healerd` without TLS, or an overly broad `HEALER_ALLOWED_HOSTS`).

## Hardening notes for operators

- Run `healerd` behind TLS and restrict who can reach the webhook and report endpoints.
- Use a dedicated bot account whose token can only open pull requests on the repositories you choose, and never grant it merge or admin rights.
- Keep `HEALER_ALLOWED_HOSTS` as narrow as possible. The daemon refuses to start when it is empty.
- Provide secrets through environment variables or `HEALER_<NAME>_FILE`, never through committed files.
- Review every `auto-fix/*` pull request before merging; `healerd` never merges on its own.

See [THREAT_MODEL.md](THREAT_MODEL.md) for the threats considered and their mitigations.
