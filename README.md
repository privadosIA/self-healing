# self-healing

A self-hosted Go daemon that turns failed CI pipelines into pull requests. It listens for failures on Gitea, reads the findings reported by SonarQube or Trivy, asks a local LLM for a small patch, and opens a pull request on an `auto-fix/*` branch for a human to review.

> **Status: proof of concept, under development.** Configuration and logging are in place; the webhook listener, scanners, LLM client and Git workflow are being built task by task.

## How it works

1. Gitea sends a `workflow_job` (or `status`) webhook when a pipeline fails.
2. `healerd` verifies the HMAC signature, drops duplicates and ignores events from `auto-fix/*` branches.
3. It fetches the findings for the failing commit from SonarQube, or from a Trivy report that your CI uploads.
4. It picks one finding and sends the LLM a small snippet around it, never the whole repository.
5. It validates the returned diff: it must apply cleanly, touch exactly one file (the finding's), and stay under 200 lines.
6. It pushes a branch and opens a pull request with the finding, before and after, and a "reviewed by a human" checklist. **It never merges.**

## Safety by design

- **Egress allowlist.** The daemon refuses to start unless `HEALER_ALLOWED_HOSTS` is set and covers every configured endpoint.
- **Untrusted inputs.** Webhooks are authenticated, and both repository content and LLM output are treated as untrusted.
- **No secret in logs.** Credentials are redacted, and request bodies and prompts are only logged at `debug` level.
- **Bounded blast radius.** One finding, one file, at most 200 lines per fix, and a cap on open auto-fix pull requests per repository.

See [SECURITY.md](SECURITY.md) and [THREAT_MODEL.md](THREAT_MODEL.md).

## Requirements

- Go 1.27.3 or later
- `git` available at runtime (included in the Docker image)
- A Gitea server, an OpenAI-compatible LLM endpoint, and optionally SonarQube and Trivy

## Build and test

```sh
make build   # static binary in bin/healerd
make test    # tests with the race detector
make lint    # gofmt, go vet, staticcheck
make vuln    # govulncheck and gosec
make docker  # build the container image
```

Run a single test with `go test ./internal/config -run TestLoad`.

## Configuration

Everything is configured through `HEALER_*` environment variables. Copy [`.env.example`](.env.example) and see [docs/configuration.md](docs/configuration.md) for the full reference.

```sh
set -a; . ./.env; set +a
bin/healerd
```

## License

[MIT](LICENSE)
