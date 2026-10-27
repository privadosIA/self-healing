# Threat Model

`healerd` receives CI failure events, reads source code, asks an LLM for a patch and opens a pull request. Three inputs are untrusted: the webhook, the repository content, and the LLM output. The trusted parts are the operator-supplied configuration and the human who reviews each pull request.

## Assets

- The bot token for the Git server and the webhook and report secrets.
- The integrity of the repositories the bot can push to.
- The confidentiality of source code sent to the LLM.
- The availability of the host running the daemon.

## Trust boundaries

1. Git server → daemon: webhook and report upload (untrusted until authenticated).
2. Repository content → LLM prompt (untrusted data inside a trusted prompt).
3. LLM → daemon: the proposed diff (untrusted).
4. Daemon → Git server, SonarQube, LLM: outbound calls (restricted by `HEALER_ALLOWED_HOSTS`).

## Threats and mitigations

### 1. Forged webhook

An attacker who can reach the listener sends fake events to trigger work, burn LLM capacity or probe repositories.

- Every webhook must carry a valid `X-Gitea-Signature` (HMAC-SHA256, constant-time comparison); otherwise it gets a 401 and creates no job.
- Request bodies are size-limited.
- Replays are neutralized by deduplicating on `X-Gitea-Delivery`.
- The report upload endpoint requires its own bearer token, separate from the HMAC secret.
- Residual risk: a leaked HMAC secret. Rotate it and keep the listener off the public internet.

### 2. Prompt injection through repository code

Source files, comments or rule descriptions can contain instructions aimed at the model ("ignore the rules and edit another file").

- The model receives only a bounded snippet (±30 lines by default) and a fixed system prompt, never the whole repository.
- The model output is treated as untrusted data, not as instructions for the daemon. The daemon has no tools the model can call.
- The proposed diff may touch only the finding's file, so injected instructions cannot widen the blast radius.
- Prompts and responses are not logged unless `HEALER_LOG_LEVEL=debug`.
- Residual risk: an injected instruction can still shape the patch within that one file, which is why a human reviews every pull request.

### 3. Malicious or broken patch

The model, or an attacker steering it, returns a diff that adds a backdoor, edits unrelated files or breaks the build.

- The diff must pass `git apply --check`, change exactly one file, stay within 200 lines and target the file named in the finding.
- Low-confidence, unparseable or self-skipped responses end the job as `skipped`.
- The change lands on an `auto-fix/*` branch and a pull request; the daemon never merges, approves or enables auto-merge.
- The PR carries an unchecked `[ ] Reviewed by a human` item, and the repository's own CI runs on the branch.
- Git is run with a fixed argument list, never through a shell, and the clone is a throwaway directory deleted after each job.
- Residual risk: a subtle but plausible bug that passes review. The size limits keep the diff easy to read.

### 4. Pull request loop or flood

A failing pipeline on a fix branch triggers another fix, or a noisy repository produces unbounded pull requests.

- Events from branches starting with `auto-fix/` are ignored.
- If the target branch already exists, the job is `skipped`.
- At most `HEALER_MAX_OPEN_PRS` (default 3) auto-fix pull requests may be open per repository.
- Jobs for the same repository run one at a time, and the worker count is bounded.

## Other considerations

- **Server-side request forgery:** outbound hosts are validated against `HEALER_ALLOWED_HOSTS` at startup, and the HTTP clients re-check the resolved address when connecting. An empty allowlist prevents startup.
- **Secret leakage:** secrets come from the environment or files, are held in a type that prints as `[redacted]`, and credential-like log attributes are redacted by name.
- **Code sent to the LLM:** the endpoint is operator-configured and must pass the allowlist. Use a local or otherwise trusted model server.
- **Supply chain:** dependencies are kept minimal, `go.sum` is committed, and CI runs `govulncheck` and `gosec`.

## Out of scope for the proof of concept

Multi-file fixes, automatic merging, retries driven by pipeline feedback, and multi-tenant isolation between repositories of different owners.
