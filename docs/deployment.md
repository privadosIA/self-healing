# Deployment

`healerd` is a long-running service. Nothing starts it: it waits for Gitea to tell it that a pipeline failed, so if it is down at that moment the failure is simply not handled. Run it somewhere that stays up, under a supervisor that restarts it: Docker with a restart policy, or systemd.

This guide covers running it on a machine of its own with Docker Compose, what must be able to reach what, and how to run it next to Gitea instead. All hosts are placeholders: replace `gitea.example.com`, `healer.example.com` and `llm.example.com` with your own, and keep your real values out of anything you commit.

## What must reach what

Four connections matter. Most failed first deployments are one of them being blocked.

| From | To | Why | What to check |
|---|---|---|---|
| Gitea | `healerd` `/webhook` | Delivers the failed-job event. | Gitea refuses private addresses by default: allow the daemon in its `[webhook] ALLOWED_HOST_LIST` (see [Connecting Gitea](gitea-webhook-setup.md)). |
| Your CI runner | `healerd` `/reports` | Uploads the Trivy report (only if you use Trivy). | The runner is usually a container: the address must work from inside it, so `localhost` does not. |
| `healerd` | Gitea | Clones, pushes the fix branch, opens the pull request. | `HEALER_GITEA_URL` must be the address Gitea uses for itself (its `ROOT_URL`), and its host must be in `HEALER_ALLOWED_HOSTS`. |
| `healerd` | the model | Asks for the patch. | The host of `HEALER_LLM_URL` must be in `HEALER_ALLOWED_HOSTS`. |

`healerd` also reaches SonarQube if you set `HEALER_SONAR_URL`. It needs no other outbound access, and the allowlist stops it from contacting anything else.

Plain `http` is accepted for all of these. Inside a network you control that is a reasonable choice; across anything else, use `https`.

## Docker Compose on a machine of its own

You need Docker with the Compose plugin, and a clone of this repository on that machine.

1. Create the configuration. The secrets live in `deploy/.env`, which git ignores:

   ```sh
   cp .env.example deploy/.env
   chmod 600 deploy/.env
   ```

   Edit it: at least `HEALER_ALLOWED_HOSTS`, `HEALER_GITEA_URL`, `HEALER_GITEA_TOKEN`, `HEALER_WEBHOOK_SECRET`, `HEALER_LLM_URL` and `HEALER_LLM_MODEL`. Generate the secrets with `openssl rand -hex 32`. Add `HEALER_REPORTS_TOKEN` if you use Trivy. [Configuration](configuration.md) describes every variable.

2. Copy the compose file and adjust it. The `environment:` block holds the non-secret settings; anything in `deploy/.env` is also read.

   ```sh
   cp deploy/docker-compose.example.yml deploy/docker-compose.yml
   ```

3. Build and start it:

   ```sh
   docker compose -f deploy/docker-compose.yml up -d --build
   docker compose -f deploy/docker-compose.yml logs -f healerd
   ```

   The log should show `healerd is listening` and nothing else. If the daemon refuses to start, the message names the variable at fault.

4. Check it from the machine itself:

   ```sh
   curl --fail http://localhost:8080/healthz
   ```

   The image also checks its own health with `healerd healthcheck`, so `docker ps` shows `healthy` once it is up.

The container runs as an unprivileged user with a read-only root filesystem, no capabilities and an in-memory `/tmp` for the clones. The only persistent data is the SQLite database in the `healerd-data` volume, mounted at `/data`.

## Reaching the daemon

The compose file publishes the port on loopback, so nothing outside the machine can reach it. Gitea and your runners must, so choose one:

**A reverse proxy on the same machine (recommended).** It terminates TLS and forwards only what is needed. `/metrics` has no authentication and must not be exposed. An example for nginx:

```nginx
server {
    listen 443 ssl;
    server_name healer.example.com;

    ssl_certificate     /etc/ssl/healer/fullchain.pem;
    ssl_certificate_key /etc/ssl/healer/privkey.pem;

    # Trivy reports may be up to 20 MiB.
    client_max_body_size 25m;

    location = /webhook   { proxy_pass http://127.0.0.1:8080; }
    location ^~ /reports/ { proxy_pass http://127.0.0.1:8080; }
    location = /healthz   { proxy_pass http://127.0.0.1:8080; }

    location / { return 404; }
}
```

**Publishing on the address of a trusted network.** If Gitea and the runners share a private network with this machine and you have no proxy, change the `ports:` line of the compose file to that address, for example `192.0.2.10:8080:8080`. Never use `0.0.0.0`. Everything on that network can then read `/metrics`, so restrict the port with the machine's firewall to the hosts that need it.

Whichever you choose, the webhook is authenticated by its HMAC signature and the report upload by its bearer token, so a request that reaches the port without the secret is rejected. They do not replace keeping the port off untrusted networks.

## The model on another machine

The model can run anywhere `healerd` can reach, as long as it speaks the OpenAI chat API. With Ollama on another host:

1. Make Ollama listen on the network. By default it listens on loopback only; set `OLLAMA_HOST=0.0.0.0:11434` in its environment and restart it.
2. Ollama has no authentication. Allow its port only from the machine that runs `healerd`.
3. Point `healerd` at it:

   ```sh
   HEALER_LLM_URL=http://llm.example.com:11434/v1
   HEALER_LLM_MODEL=<a name shown by `ollama list`>
   HEALER_ALLOWED_HOSTS=example.com
   ```

Some things to expect:

- `healerd` asks the model for the line range to replace and the new text for it, and builds the diff itself, so a small model does not have to count diff lines. Quality still depends on the model. A code-tuned model of about 14 billion parameters produced a usable fix in one sandbox run; smaller or general-purpose models may answer with something the daemon rejects, and the job then ends `skipped` with nothing pushed.
- A 14B model needs around 9 GB of memory while loaded, and Ollama keeps it loaded for several minutes after a request (`OLLAMA_KEEP_ALIVE`). Do not share that machine with other heavy work if memory is tight.
- On CPU or modest hardware a request can take minutes. Raise `HEALER_LLM_TIMEOUT` and `HEALER_JOB_TIMEOUT` if jobs end `failed` with a timeout.

## Running next to Gitea

Putting `healerd` on the Gitea machine is fine and removes the network hop for the webhook. Either way works:

- **Docker Compose**, as above. Publish the port on loopback and let Gitea call `localhost`; add `localhost` to `[webhook] ALLOWED_HOST_LIST` in Gitea's `app.ini`, which is also needed because Gitea refuses private addresses by default.
- **systemd**, with [`deploy/healerd.service`](../deploy/healerd.service), a hardened unit. Install the binary from `make build`, put the environment in a file readable only by the service user and referenced by the unit, and enable it with `systemctl enable --now healerd`.

If your runners are containers on the same host, give them an address for the daemon that works from inside the container, for example the host's address on the Docker bridge, not `localhost`.

## Upgrades, backups and logs

- **Upgrade.** Pull the new source, then rebuild and restart:

  ```sh
  git pull
  docker compose -f deploy/docker-compose.yml up -d --build
  ```

  On `SIGTERM` the daemon lets running jobs finish for up to `HEALER_SHUTDOWN_GRACE`, and the compose file waits longer than that before killing it. A job cut short anyway is marked `failed` at the next start and is never retried, because it may already have opened a pull request. Jobs that had not started stay queued and run after the restart.
- **Backup.** The database holds the job history, the deliveries already handled and the 500 most recent Trivy reports. It is operational data, not irreplaceable: if it is lost, `healerd` forgets which deliveries it handled and the stored reports, but it still refuses to push a fix branch that already exists, so the same rule and commit does not get a second pull request. To back it up, copy the file while the daemon is stopped, or use `sqlite3 /data/healerd.db ".backup /tmp/healerd-backup.db"`.
- **Logs.** The daemon writes JSON lines to standard output, so `docker compose logs` or the journal is enough. Leave `HEALER_LOG_LEVEL` at `info`: at `debug` the log contains prompts and model answers, which contain your code.
- **Metrics.** `GET /metrics` serves Prometheus metrics. Scrape it from the machine itself or over a trusted network, never through the public proxy.

## Security checklist

- The bot account has **Write** access to the repositories you want healed and nothing else. It is not an administrator and cannot merge; protect the default branch so that a review is required.
- Secrets are in `deploy/.env` with mode `600`, or in files referenced with the `_FILE` variants, never in the compose file or the image. The repository ignores both.
- The port is not reachable from untrusted networks, and `/metrics` is not exposed through the proxy.
- `HEALER_ALLOWED_HOSTS` lists only the hosts `healerd` needs, so a misconfiguration cannot send source code elsewhere.
- Ollama, if you use it, is reachable only from the machine that runs `healerd`.
- You tested it once end to end in a sandbox repository before pointing it at real ones: see [`scripts/e2e.md`](../scripts/e2e.md).
