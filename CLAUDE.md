# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`healerd`, a self-hosted Go daemon: a failed Gitea pipeline arrives as a webhook, it reads findings from SonarQube or an uploaded Trivy report, asks an OpenAI-compatible model for a patch, validates it, and opens a pull request on an `auto-fix/*` branch. It never merges. **This repository is public**: see the rules at the end before adding anything.

## Commands

```sh
make build        # static binary in bin/healerd
make test         # go test ./... -race with coverage
make lint         # gofmt check, go vet, staticcheck, golangci-lint (pinned)
make vuln         # govulncheck and gosec (pinned)
make docker       # image, needs Docker

go test ./internal/config -run TestLoad          # one test
go test ./internal/core -race -count=30          # stress the concurrency tests
```

Run `make lint test vuln build` before every commit; CI runs the same targets. The tests that use git (`internal/gitcli`, `internal/adapters/gitea`, `internal/app`, `cmd/healerd`) need the `git` executable, and the last three also `git http-backend`; they skip when it is missing. Docker and systemd are not needed to test.

## Architecture

Hexagonal. `internal/core` holds the use case and depends only on `internal/ports` and `internal/core/model`; a test (`internal/core/arch_test.go`) fails if the core imports network, disk, database or adapter packages. Everything else plugs in through the ports.

```
cmd/healerd            signals, exit codes; `healerd healthcheck` for the container
internal/app           assembles everything from config; the only place that knows all parts
internal/config        HEALER_* environment, allowlist, Secret type
internal/core          Healer (use case), Pool (workers), Selector, BuildPullRequest
internal/core/model    domain types, branch naming
internal/ports         interfaces: JobStore, ReportStore, Scanner, LLM, VCS/Workspace, Clock
internal/listener      HTTP: HMAC middleware, webhook, report upload, health, metrics
internal/adapters/     sonar, trivy, llm/openai, gitea (API + git workspace), store/sqlite
internal/gitcli        the only code that runs git
internal/safehttp      HTTP clients that enforce the egress allowlist
internal/glob          ** globs for HEALER_EXCLUDE_GLOBS
internal/logging       slog JSON with redaction
```

The flow, in the order `Healer.run` does it (cheap checks first, so no checkout or model call is wasted): count open auto-fix PRs, gather findings from every scanner, select one, check out the commit, skip if the fix branch exists, read the file, ask the model, commit the patch, push, open the PR. Outcomes are `pr_opened`, `skipped` (nothing safe to do) or `failed`; an unusable model answer wraps `ports.ErrNoProposal` and a patch git cannot use wraps `ports.ErrPatchRejected`, both meaning skipped.

`Pool` serializes jobs per repository with a queue per repository, not a lock, so a busy repository cannot hold workers. Jobs left `running` by a crash are marked failed at startup and never retried (they may already have opened a PR); `queued` ones are run, by a sweep of the store.

## Things that are easy to get wrong

- **Scanner and model text is untrusted.** It goes into commit messages, PR titles and bodies, so it must pass `oneLine`/`codeSpan`/`fenced` (and `FormatCommitMessage`), never be interpolated raw.
- **Git only through `internal/gitcli`**, never a shell. It strips ambient `GIT_*`, proxies and user config, passes the token through the environment only, and scrubs it from errors. Repository files are read through `os.Root` so a symlink cannot escape the workspace.
- **Every outbound HTTP client comes from `safehttp.NewClient`.** Git does not, so the Gitea adapter checks the clone URL itself.
- **No merge path.** `TestAdapterNeverCallsMergeEndpoints` inspects the Gitea adapter's source; do not add PUT, PATCH, DELETE or merge/approve/review strings there.
- **Adding a configuration variable means touching three places**, or tests fail: `internal/config/config.go`, `.env.example` and `docs/configuration.md`. The deploy files and docs may only name variables that exist.
- **Changing a port** means updating the fakes in `internal/core/heal_test.go` and the adapters.
- `#nosec` is allowed only with a one-line justification (see `config.go` and `cmd/healerd/main.go`).
- The webhook and Gitea payload shapes are modelled on Gitea's documentation, not copied from an instance; `scripts/e2e.md` is how to verify them.

## Rules for this public repository

- **No real infrastructure data**: no internal host names, private addresses, user names, repository names or home paths in code, tests, fixtures, docs or commits. Use `example.com` hosts, `acme/widget`, `ci-healer-bot` and the reserved range `192.0.2.0/24`. `hygiene_test.go` scans the whole repository for this, and `docs/docs_test.go` checks the documentation.
- **No secrets**: only environment variables or files named by them; examples use `changeme` or are empty.
- **All documentation and comments are in English**, including the README.
- Before committing, review `git diff --cached`; if anything might be real, stop and ask.
- Fixtures are invented or hand-anonymised, never copied from a real instance.
- Logs never print webhook bodies, tokens, prompts or model answers (only at `debug`, with credentials redacted).

## Workflow

Work is done one task at a time: the tests land with the task, then `make lint test vuln build`, then one conventional commit in English. Keep the documentation in `docs/` in step with the code.
