# Configuration

`healerd` is configured only through environment variables prefixed with `HEALER_`. [`.env.example`](../.env.example) is a ready-to-copy template with placeholder values.

The daemon validates everything at startup and reports **all** problems at once. Errors name the variable but never print its value.

## Secrets from files

Any secret can be supplied either directly or from a file, which suits Docker and Kubernetes secrets:

- `HEALER_GITEA_TOKEN=...`, or
- `HEALER_GITEA_TOKEN_FILE=/run/secrets/gitea_token`

Setting both for the same secret is an error. Surrounding whitespace in the file is trimmed. This applies to `HEALER_GITEA_TOKEN`, `HEALER_WEBHOOK_SECRET`, `HEALER_REPORTS_TOKEN`, `HEALER_SONAR_TOKEN` and `HEALER_LLM_API_KEY`.

## Variables

### Server

| Variable | Required | Default | Description |
|---|---|---|---|
| `HEALER_LISTEN_ADDR` | no | `:8080` | Address the HTTP server listens on. |
| `HEALER_DB_PATH` | no | `healerd.db` | Path of the SQLite database that stores jobs. The Docker image sets `/data/healerd.db`. |
| `HEALER_LOG_LEVEL` | no | `info` | One of `debug`, `info`, `warn`, `error`. Request bodies, prompts and LLM responses are only logged at `debug`, and credentials are always redacted. |

### Egress allowlist

| Variable | Required | Default | Description |
|---|---|---|---|
| `HEALER_ALLOWED_HOSTS` | **yes** | none | Comma-separated domain suffixes and CIDRs the daemon may contact. |

The daemon refuses to start if this list is empty, or if the host of `HEALER_GITEA_URL`, `HEALER_SONAR_URL` or `HEALER_LLM_URL` is not covered by it. This is deliberate: it stops a misconfiguration from sending source code to an unexpected host.

- A suffix such as `example.com` matches `example.com` and any subdomain (`gitea.example.com`), but not `evilexample.com`.
- A CIDR or single IP such as `10.0.0.0/8` or `192.0.2.7` matches endpoints given as IP addresses.
- Hostnames are matched against suffixes only. The HTTP clients apply the same rules to every connection they open, so a redirect to a host outside the list is refused. They also ignore the `HTTP_PROXY` and `HTTPS_PROXY` variables, because a proxy would hide the real destination.

### Gitea

| Variable | Required | Default | Description |
|---|---|---|---|
| `HEALER_GITEA_URL` | **yes** | none | Base URL of the Gitea server, for example `https://gitea.example.com`. Must be `http(s)` and must not embed credentials. |
| `HEALER_GITEA_TOKEN` | **yes** | none | Token of the bot account used to clone, push and open pull requests. |
| `HEALER_WEBHOOK_SECRET` | **yes** | none | Shared secret used to verify the HMAC-SHA256 signature of incoming webhooks. |

### Trivy reports

| Variable | Required | Default | Description |
|---|---|---|---|
| `HEALER_REPORTS_TOKEN` | no | empty | Bearer token required to upload reports with `PUT /reports/{repo_id}/{sha}`. Leaving it empty disables the endpoint. |

### SonarQube

| Variable | Required | Default | Description |
|---|---|---|---|
| `HEALER_SONAR_URL` | no | empty | Base URL of SonarQube. Leaving it empty disables the Sonar scanner. |
| `HEALER_SONAR_TOKEN` | if `HEALER_SONAR_URL` is set | empty | Token used to read issues and rules. |

### LLM

| Variable | Required | Default | Description |
|---|---|---|---|
| `HEALER_LLM_URL` | **yes** | none | Base URL of an OpenAI-compatible API, for example `http://llm.example.com/v1`. |
| `HEALER_LLM_API_KEY` | no | empty | API key sent to the LLM server, if it requires one. |
| `HEALER_LLM_MODEL` | **yes** | none | Model name or alias sent in the `model` field. |

### Commit author

| Variable | Required | Default | Description |
|---|---|---|---|
| `HEALER_GIT_AUTHOR_NAME` | no | `ci-healer-bot` | Name on the commits `healerd` creates. |
| `HEALER_GIT_AUTHOR_EMAIL` | no | `ci-healer-bot@users.noreply.invalid` | Email on those commits. Use the bot account's email if you want Gitea to link the commits to it. |

### Timeouts

Values are Go durations such as `90s` or `5m`.

| Variable | Required | Default | Description |
|---|---|---|---|
| `HEALER_LLM_TIMEOUT` | no | `5m` | How long one request to the LLM may take. A local model can be slow. |
| `HEALER_JOB_TIMEOUT` | no | `15m` | Upper bound for a whole job (scan, clone, model call, push, pull request). A stuck job is cancelled and recorded as failed. |
| `HEALER_SHUTDOWN_GRACE` | no | `30s` | On SIGINT or SIGTERM, how long running jobs may finish before they are cancelled. Jobs that had not started stay queued for the next start. |

### Behavior

| Variable | Required | Default | Description |
|---|---|---|---|
| `HEALER_WORKERS` | no | `2` | Number of jobs processed in parallel. Jobs of the same repository always run one at a time. Minimum 1. |
| `HEALER_MAX_OPEN_PRS` | no | `3` | Maximum number of open `auto-fix` pull requests per repository. Further failures are skipped until some are closed. Minimum 1. |
| `HEALER_SNIPPET_CONTEXT_LINES` | no | `30` | Lines of context sent to the LLM before and after the finding. Minimum 1. |
| `HEALER_EXCLUDE_GLOBS` | no | `**/*.lock,**/migrations/**,**/*secret*` | Comma-separated globs of files that are never modified. |
