# End-to-end check against a sandbox

The automated tests drive the whole daemon against a real git server and *fake* Gitea, SonarQube and model endpoints. They cannot tell you whether your real Gitea sends the payloads `healerd` expects, or whether your model produces patches that survive validation. This procedure does. Run it once on your own instance before you rely on `healerd`, and again after upgrading Gitea.

Everything here uses placeholders: `gitea.example.com`, `sonar.example.com`, `llm.example.com`, `healer.example.com`, the repository `acme/sandbox` and the bot `ci-healer-bot`. Replace them with yours, and keep your real values out of anything you commit or share.

## 0. Ground rules

- **Use a sandbox repository** that nobody depends on, and a bot account that has Write access to it and to nothing else.
- **Use throwaway tokens** and revoke them at the end.
- `HEALER_LOG_LEVEL=debug` writes prompts and model answers, which contain your code, to the log. Use it in the sandbox only.

## 1. Prepare the sandbox repository

1. Create `acme/sandbox` in Gitea with a `main` branch and branch protection that requires a review to merge.
2. Add a small Go module (any language works; Go keeps this example short) with a file that SonarQube or Trivy will flag. Either:
   - a function with an obvious code smell that your SonarQube rules report, or
   - a dependency pinned to an old release that Trivy reports as vulnerable.
3. Add a pipeline that runs the scanner and then fails. For Trivy, use the workflow in [`docs/ci-integration.md`](../docs/ci-integration.md), including the upload step *before* the step that fails. For SonarQube, run the scan before the failing step and set `sonar.projectKey` to `sandbox`.
4. Note the repository's numeric id (see the same guide).

## 2. Start `healerd`

```sh
export HEALER_ALLOWED_HOSTS=example.com
export HEALER_GITEA_URL=https://gitea.example.com
export HEALER_GITEA_TOKEN=changeme            # the bot's token
export HEALER_WEBHOOK_SECRET=changeme         # openssl rand -hex 32
export HEALER_REPORTS_TOKEN=changeme          # only if you use Trivy
export HEALER_SONAR_URL=https://sonar.example.com   # only if you use SonarQube
export HEALER_SONAR_TOKEN=changeme
export HEALER_LLM_URL=http://llm.example.com/v1
export HEALER_LLM_MODEL=changeme
export HEALER_MAX_OPEN_PRS=3
export HEALER_LOG_LEVEL=debug

bin/healerd
```

Expected: a `healerd is listening` line and nothing else. Check it from another shell:

```sh
curl --fail https://healer.example.com/healthz     # ok
```

## 3. Connect the webhook and send a test delivery

Create the webhook as described in [`docs/gitea-webhook-setup.md`](../docs/gitea-webhook-setup.md) and press *Test Delivery*.

| Expected | Where |
|---|---|
| `204` | Gitea's *Recent Deliveries* |
| a log line `event ignored` | `healerd` |

A `401` means the secret differs. A connection error means Gitea cannot reach `healerd` (see Gitea's `[webhook] ALLOWED_HOST_LIST`).

## 4. Replay a delivery by hand (optional, no pipeline needed)

This exercises everything except your CI. Copy the body of a real failed `workflow_job` delivery from Gitea's *Recent Deliveries*, or use this shape, and sign it:

```sh
BODY='{"action":"completed","workflow_job":{"id":1,"run_id":2001,"name":"quality","head_sha":"0123456789abcdef0123456789abcdef01234567","head_branch":"main","status":"completed","conclusion":"failure"},"repository":{"id":42,"full_name":"acme/sandbox","clone_url":"https://gitea.example.com/acme/sandbox.git","default_branch":"main"}}'
SIG=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac "$HEALER_WEBHOOK_SECRET" -hex | sed 's/^.* //')

curl -i -X POST https://healer.example.com/webhook \
  -H "Content-Type: application/json" \
  -H "X-Gitea-Event: workflow_job" \
  -H "X-Gitea-Delivery: manual-1" \
  -H "X-Gitea-Signature: ${SIG}" \
  --data "$BODY"
```

Use the real commit id and repository id of your sandbox. Expect `202`; sending the same command again must give `200` and do nothing.

## 5. Fail the pipeline for real

Push a commit that triggers the failing pipeline, then watch.

1. **Webhook.** Gitea's *Recent Deliveries* shows `202`. In the log: `job queued`.
2. **Job.** After a while the log shows `job finished` with `status=pr_opened`.
3. **Metrics.**
   ```sh
   curl --silent https://healer.example.com/metrics | grep -E '^healerd_(webhooks|jobs|reports)_total'
   ```
   `healerd_webhooks_total{result="queued"}` and `healerd_jobs_total{status="pr_opened"}` each went up by one.
4. **The pull request.** Open it and check each item:

   - [ ] Branch is `auto-fix/<rule>/<7 hex digits>`, and the base is the branch that failed.
   - [ ] Title is `[auto-fix] <rule>: <summary>`.
   - [ ] Labels `auto-fix` and the scanner name (`sonar` or `trivy`) are set.
   - [ ] The body shows the finding table, the *Before* and *After* blocks, links, and an **unchecked** *Reviewed by a human* item.
   - [ ] **One** file changed, and it is the finding's file; the diff is short.
   - [ ] The commit message is `fix(<source>): <rule> in <file>` followed by the `Finding-Source`, `Finding-Rule` and `Triggered-By` trailers.
   - [ ] The commit's author is the bot, and the commit is not signed.
   - [ ] The default branch did not move, and nothing was merged.

5. **Does the fix make sense?** Read the diff as a reviewer would. If you close the pull request, `healerd` will not reopen it: the next failure of the same commit is skipped because the branch exists, and a new commit gives a new branch.

## 6. Negative checks

Each row is a small experiment. Run the ones that matter to you.

| Do this | Expect |
|---|---|
| Send the replay from step 4 with a wrong signature | `401`; nothing in the log but a rejection; no job |
| Redeliver a delivery from Gitea's UI | `200`; no second job |
| Fail the pipeline on the `auto-fix/...` branch of an open pull request | `204`; log `event ignored`; no new job |
| Set `HEALER_MAX_OPEN_PRS=1` and fail a second commit while one pull request is open | the job ends `skipped`, reason mentions the limit; no checkout |
| Fail the same commit again after a pull request exists | `skipped`, reason says the branch already exists |
| Point `HEALER_LLM_URL` at a closed port | the job ends `failed`; **no branch is left on Gitea** |
| Make the model answer with prose or a diff for another file | the job ends `skipped`; nothing is pushed |
| Upload a Trivy report with a wrong token | `401`; with `{}` as the body, `400` |
| Fail the pipeline *without* uploading the Trivy report | `skipped` (no fixable finding) |
| Try to merge with the bot's token: `curl -X POST -H "Authorization: token ${BOT_TOKEN}" -H "Content-Type: application/json" -d '{"Do":"merge"}' https://gitea.example.com/api/v1/repos/acme/sandbox/pulls/1/merge` | refused (`403`) by branch protection; `healerd` never calls this endpoint |
| Start `healerd` with an empty `HEALER_ALLOWED_HOSTS`, or with `HEALER_LLM_URL` on a host outside it | it refuses to start and names the variable |
| Send `SIGTERM` while a job runs | `shutting down`, the job finishes, `stopped`, exit status 0 |
| Kill the process with `SIGKILL` during a job, then start it again | the job is marked `failed` (*interrupted by a restart*) and **not retried**; queued jobs run |

## 7. Compare the payload with what `healerd` expects

The fixtures in `testdata/gitea/` were written from Gitea's documented webhook format, not copied from an instance. In the log (at `debug`) or in Gitea's *Recent Deliveries*, confirm that your Gitea's real payload has these fields, with these names:

| Needed | Field |
|---|---|
| event type | the `X-Gitea-Event` header (`workflow_job` or `status`) |
| delivery id | the `X-Gitea-Delivery` header |
| signature | the `X-Gitea-Signature` header |
| outcome | `workflow_job.conclusion` (`failure`), or `state` for `status` |
| commit | `workflow_job.head_sha`, or `sha` for `status` |
| branch | `workflow_job.head_branch`, or `branches[0].name` for `status` |
| job and run | `workflow_job.name`, `workflow_job.run_id` |
| repository | `repository.id`, `.full_name`, `.clone_url`, `.default_branch` |

If a name differs, adjust the structs in `internal/listener/webhook.go` and add an **anonymised** copy of the real payload to `testdata/gitea/`: replace the repository, owner, host, commit and every user name with placeholders such as `acme/widget`, `gitea.example.com` and `0123456…`, and never paste a token. The repository's tests will then guard the format you actually use.

## 8. Before you share anything

Logs, screenshots and payload samples from this exercise contain your hosts, repository names and code. Redact them first. If you add anything to this repository, run:

```sh
make test      # includes the repository hygiene check
```

The hygiene test fails if a tracked file contains an internal host name, a private address (outside test inputs), an email address that is not a placeholder, a home-directory path, or something that looks like a token.

## 9. Clean up

Delete the sandbox repository and the webhook, revoke the bot's token and the reports token, and remove the database file (`HEALER_DB_PATH`) if you used a throwaway one.
