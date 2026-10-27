# Connecting Gitea

This guide sets up Gitea so that a failed pipeline reaches `healerd`, and so that `healerd` can open a pull request in return. All hosts below are placeholders: replace `gitea.example.com` and `healer.example.com` with your own.

```
Gitea ──webhook (workflow_job failed)──▶ healerd ──clone, push, pull request──▶ Gitea
                                           │
                                           ├──▶ SonarQube (issues)
                                           └──▶ LLM endpoint (patch)
```

## 1. Create the bot account

Create a dedicated user, for example `ci-healer-bot`, and never reuse a human's account.

1. Give it **Write** access (and no more) to each repository you want healed, either as a collaborator or through a team. Do not make it an owner or an administrator.
2. Sign in as the bot and create an access token under *Settings → Applications*. It needs permission to read and write repositories, and to write issues and pull requests (the pull request labels are created through the issues API). Gitea's scope names are `write:repository` and `write:issue`; check them against your Gitea version.
3. Put the token in `HEALER_GITEA_TOKEN` (or in a file referenced by `HEALER_GITEA_TOKEN_FILE`).

`healerd` only ever creates branches named `auto-fix/...` and opens pull requests. It has no code that merges, approves or enables auto-merge, so it needs no merge rights. Keep it that way:

- Protect the default branch and require a review before merging.
- If your Gitea lets you restrict who may push, allow the bot to push only branches matching `auto-fix/*`.

## 2. Configure `healerd`

At minimum:

```sh
HEALER_ALLOWED_HOSTS=example.com
HEALER_GITEA_URL=https://gitea.example.com
HEALER_GITEA_TOKEN=changeme
HEALER_WEBHOOK_SECRET=changeme
HEALER_LLM_URL=http://llm.example.com/v1
HEALER_LLM_MODEL=changeme
```

Generate the webhook secret with a good random source, for example `openssl rand -hex 32`. See [configuration.md](configuration.md) for every variable.

Two details that cause most first-time failures:

- **`HEALER_GITEA_URL` must be the address Gitea uses for itself.** The webhook carries the repository's `clone_url`, which Gitea builds from its `ROOT_URL`. `healerd` refuses to clone from any host or port other than `HEALER_GITEA_URL`'s, so if the two differ the job ends with *the clone URL is not on the configured Gitea host*. Make them match, or set Gitea's `ROOT_URL` to the address `healerd` can reach.
- **The host must be in `HEALER_ALLOWED_HOSTS`.** The daemon refuses to start otherwise.

## 3. Let Gitea call `healerd`

By default Gitea refuses to deliver webhooks to private addresses. If `healerd` runs on your internal network, allow it in Gitea's `app.ini`:

```ini
[webhook]
ALLOWED_HOST_LIST = healer.example.com
```

Restart Gitea afterwards. Place `healerd` behind a reverse proxy that terminates TLS and forwards `/webhook` (and `/reports`, if you use Trivy). The daemon itself speaks plain HTTP.

## 4. Add the webhook to a repository

In the repository, open *Settings → Webhooks → Add Webhook → Gitea* and fill in:

| Field | Value |
|---|---|
| Target URL | `https://healer.example.com/webhook` |
| HTTP method | `POST` |
| POST content type | `application/json` |
| Secret | the value of `HEALER_WEBHOOK_SECRET` |
| Trigger on | *Custom events*: the workflow job event (and, optionally, commit status) |
| Active | checked |

`healerd` handles two events and ignores everything else:

| Event | Acts on |
|---|---|
| `workflow_job` | a job whose `conclusion` is `failure` |
| `status` | a commit status whose `state` is `failure` or `error` |

The payload formats are modelled on Gitea's documented webhook format, and the names of the events in the UI and the API depend on your Gitea version. Because versions differ, run the end-to-end check described in `scripts/e2e.md` against your instance before relying on it.

You can create the same webhook through the API instead of the UI:

```sh
curl --fail -X POST "https://gitea.example.com/api/v1/repos/acme/widget/hooks" \
  -H "Authorization: token ${GITEA_ADMIN_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
        "type": "gitea",
        "active": true,
        "events": ["workflow_job"],
        "config": {
          "url": "https://healer.example.com/webhook",
          "content_type": "json",
          "secret": "changeme"
        }
      }'
```

## 5. What `healerd` does with a delivery

1. It checks the `X-Gitea-Signature` header, the hex HMAC-SHA256 of the body under your secret. A missing or wrong signature gets `401` and nothing else happens.
2. It ignores events on branches starting with `auto-fix/`, so a failing fix cannot trigger another fix.
3. It records the delivery by its `X-Gitea-Delivery` id. **A delivery it has already seen is ignored**, so *Redeliver* in Gitea's UI does not run the job again. To retry, push a new commit.
4. It answers straight away and does the work in the background.

Responses you will see in Gitea's *Recent Deliveries*:

| Status | Meaning |
|---|---|
| `202` | The job was queued. |
| `200` | The delivery was already known. |
| `204` | The event is valid but needs no job (not a failure, another event type, or an `auto-fix/` branch). |
| `400` | The payload lacks the repository, the commit or the branch, or `X-Gitea-Delivery` is missing. |
| `401` | The signature is missing or wrong. |
| `413` | The body is larger than 4 MiB. |

## 6. Check that it works

```sh
curl --fail https://healer.example.com/healthz          # ok
curl --silent https://healer.example.com/metrics | grep '^healerd_'
```

Use *Test Delivery* on the webhook. That sends a push event, which `healerd` ignores: Gitea shows `204` and the log says `event ignored`. Then fail a pipeline on purpose and watch the counters:

- `healerd_webhooks_total{result="queued"}` goes up when a job is accepted.
- `healerd_jobs_total{status="pr_opened" | "skipped" | "failed"}` counts how jobs end.

## 7. Why a job ends `skipped` or `failed`

`skipped` means there was nothing safe to do, `failed` means something broke. The reason is stored with the job and logged.

| Outcome | Typical reason |
|---|---|
| skipped | The repository already has `HEALER_MAX_OPEN_PRS` open auto-fix pull requests. |
| skipped | No finding to fix: the scanners reported nothing, or everything matches `HEALER_EXCLUDE_GLOBS`. For Trivy, the report was not uploaded before the webhook arrived (see [ci-integration.md](ci-integration.md)). |
| skipped | The fix branch for this rule and commit already exists. |
| skipped | The model declined, was not confident enough (below 0.6), or answered in a way that could not be used. |
| skipped | The patch does not apply, changes more than the finding's file, or is longer than 200 lines. |
| failed | A scanner, Gitea, git or the model endpoint could not be reached, or returned an error. |
| failed | The job ran longer than `HEALER_JOB_TIMEOUT`, or the daemon was restarted while it ran. |

When a pull request cannot be opened after the branch was pushed, `healerd` deletes the branch again so that the next failure of the same commit is not mistaken for already handled.

## Fetching a commit by id

`healerd` fetches the failing commit by its id, without history. That works with Git's protocol version 2, which Gitea's `git` uses by default. If your server is configured to refuse fetching unadvertised commits, make sure the failing commit is the tip of its branch when the webhook arrives, or enable `uploadpack.allowAnySHA1InWant` on the server.
