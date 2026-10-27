# self-healing

A self-hosted Go daemon that turns failed CI pipelines into pull requests. It listens for failures on Gitea, reads the findings reported by SonarQube or Trivy, asks a local LLM for a small patch, and opens a pull request on an `auto-fix/*` branch for a human to review.

> **Status: proof of concept.** The whole flow is implemented and covered by automated tests, including an end-to-end test against a real git server. It has not yet been run against a production Gitea, SonarQube and model: the webhook payloads are modelled on Gitea's documented format, so run the end-to-end check in [`scripts/e2e.md`](scripts/e2e.md) on your own instance before relying on it.

## How it works

1. Gitea sends a `workflow_job` (or `status`) webhook when a pipeline fails.
2. `healerd` verifies the HMAC signature, drops duplicates and ignores events from `auto-fix/*` branches.
3. It fetches the findings for the failing commit from SonarQube, or from a Trivy report that your CI uploads.
4. It picks one finding and sends the LLM a small snippet around it, never the whole repository.
5. It validates the returned diff: it must apply cleanly, touch exactly one file (the finding's), and stay under 200 lines.
6. It pushes a branch and opens a pull request with the finding, before and after, and a "reviewed by a human" checklist. **It never merges.**

## Safety by design

- **Egress allowlist.** The daemon refuses to start unless `HEALER_ALLOWED_HOSTS` is set and covers every configured endpoint. Every outbound connection is checked again when it is made, which also covers redirects.
- **Untrusted inputs.** Webhooks are authenticated, and both repository content and LLM output are treated as untrusted. Text from scanners and the model is shown in code spans and fences it cannot close.
- **No secret in logs.** Credentials are redacted, and request bodies and prompts are only logged at `debug` level.
- **Bounded blast radius.** One finding, one file, at most 200 lines per fix, and a cap on open auto-fix pull requests per repository.
- **No merge path.** The code has no call that merges, approves or enables auto-merge, and a test enforces it.

See [SECURITY.md](SECURITY.md) and [THREAT_MODEL.md](THREAT_MODEL.md).

## Requirements

- Go 1.27.3 or later
- `git` available at runtime (included in the Docker image)
- A Gitea server, an OpenAI-compatible LLM endpoint, and optionally SonarQube and Trivy

## Quick start

```sh
cp .env.example .env     # then edit it: hosts, tokens, model name
make build
set -a; . ./.env; set +a
bin/healerd
```

Then connect it to Gitea and your pipelines:

1. [Connecting Gitea](docs/gitea-webhook-setup.md): the bot account, the webhook and the settings that must agree.
2. [CI integration](docs/ci-integration.md): how to publish Trivy reports and what SonarQube needs.
3. [Configuration](docs/configuration.md): every `HEALER_*` variable.

## Deployment

- [`deploy/healerd.service`](deploy/healerd.service): a hardened systemd unit.
- [`deploy/docker-compose.example.yml`](deploy/docker-compose.example.yml): Docker Compose with a read-only root filesystem and the port on loopback only.
- `make docker` builds the image, which runs as an unprivileged user and checks its own health with `healerd healthcheck`.

`healerd` speaks plain HTTP and exposes `/metrics` without authentication: put a TLS-terminating reverse proxy in front of it and expose only `/webhook` (and `/reports` if you use Trivy) to the networks that need them.

## Operations

| Endpoint | Purpose |
|---|---|
| `POST /webhook` | Gitea deliveries, authenticated by HMAC. |
| `PUT /reports/{repo_id}/{sha}` | Trivy report upload, authenticated by a bearer token. Present only when `HEALER_REPORTS_TOKEN` is set. |
| `GET /healthz` | Liveness. |
| `GET /metrics` | Prometheus metrics: `healerd_webhooks_total`, `healerd_reports_total`, `healerd_jobs_total` and the Go runtime. |

On `SIGINT` or `SIGTERM` the daemon stops taking requests, lets running jobs finish for `HEALER_SHUTDOWN_GRACE`, and exits. Jobs that had not started stay queued and run on the next start; jobs cut short by a crash are marked failed and never retried, because they may already have opened a pull request.

## Build and test

```sh
make build   # static binary in bin/healerd
make test    # tests with the race detector
make lint    # gofmt, go vet, staticcheck, golangci-lint
make vuln    # govulncheck and gosec
make docker  # build the container image
```

Run a single test with `go test ./internal/config -run TestLoad`. The tests that exercise git need the `git` executable, and the end-to-end tests also use `git http-backend`; they skip themselves when it is missing.

## License

[MIT](LICENSE)
