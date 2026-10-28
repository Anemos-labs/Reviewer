# Reviewer

A pull request review bot for GitHub, run by [Sleipnir](https://github.com/anemos-labs/sleipnir). For each pull request it runs
a Sleipnir **review**: a team of agents maps the files the change wrote, hunts them for the holes the change brings in, and
reports only what other agents verified and, from the depth's threshold up, a reproducer showed in a Docker sandbox. It is the
deep scan (`sleipnir scan`, docs/SCAN.md in Sleipnir) on a diff.

What a pull request gets:

- A **walkthrough** comment, kept up to date in place: what each part of the change does (the review's maps), what was
  reviewed, what came of it, the findings that verifiers confirmed but nobody reproduced yet, and the run's details.
- A **review** (`COMMENT`, never an approval or a block) whose inline comments are the confirmed findings, each on the line
  where the change made it possible: severity and category, the claim, the trace the harness checked with links to the commit,
  how it was checked (verdicts, the defender, the reproducer and its output), the suggested fix and a prompt for an agent.
- A finding is posted once: a later push that leaves it in place does not post it again.

Comment `@reviewer review` (the name is `REVIEWER_BOT`) on a pull request to review it again. Only the repository's owners,
members and collaborators can; a review costs model time.

## How it runs

```text
GitHub ──webhook──▶ reviewer ──queue──▶ worker: clone the head ─▶ sleipnir scan --start --diff BASE
                                                               ─▶ sleipnir swarm N --sandbox docker "<goal>"
                                                               ─▶ findings.json ─▶ walkthrough + review
```

- One review per pull request at a time. A push stops the review running for an older commit; requests that wait for the same
  pull request collapse into the newest.
- The clone is made on the reviewer's side with git's hooks, fsmonitor and symbolic links off. The GitHub installation token
  reaches git through its environment for the fetch only: it is never in a command line, in the clone's config, or in the
  sleipnir process's environment (which gets only `PATH`, `HOME`, the Docker host and `REVIEWER_PASS_ENV`).
- Every command the agents run (builds, tests, reproducers) runs in Sleipnir's per-session Docker sandbox: no network unless
  `REVIEWER_SANDBOX_NETWORK=true`, every capability dropped, no host environment, only the clone mounted.

## Setup

1. Create a GitHub App with:
   - Webhook URL `https://YOUR-HOST/webhook` and a webhook secret.
   - Repository permissions: Pull requests (read and write), Issues (read and write), Contents (read), Metadata (read).
   - Events: Pull request, Issue comment.
2. Generate a private key for the app, and install the app on the repositories to review.
3. On a machine with Docker that is dedicated to this service (the reviewer drives the host's Docker engine, which is root on
   the host):
   ```sh
   cp .env.example .env              # fill GITHUB_APP_ID, HEIMDALLM_API_KEY and the review settings
   cp ~/Downloads/app.private-key.pem secrets/github-app.pem
   printf %s 'the webhook secret' > secrets/webhook-secret
   sudo mkdir -p /srv/reviewer/work
   docker compose up -d --build
   ```
   The work directory has the same path on the host and in the container, because the sandbox mounts the clone by its path.
4. `GET /healthz` answers `ok` with the number of reviews waiting.

The image builds Sleipnir from source (`SLEIPNIR_REF`, default `main`; reviews need `sleipnir scan --start --diff`) and turns
its `scan` plugin on.

## Configuration

| Variable | Default | |
|---|---|---|
| `GITHUB_APP_ID` | | The app's numeric id |
| `GITHUB_APP_PRIVATE_KEY` / `_FILE` | | The app's PEM key, or a file holding it |
| `GITHUB_WEBHOOK_SECRET` / `_FILE` | | The webhook secret, or a file holding it |
| `GITHUB_API_URL` | `https://api.github.com` | |
| `REVIEWER_ADDR` | `:8080` | Where the webhook is served |
| `REVIEWER_WORKERS` | `1` | Reviews that run at once |
| `REVIEWER_TEAM` | `8` | Agents per review (8 to 32: a Sleipnir team has at least 8 workers) |
| `REVIEWER_DEPTH` | `standard` | `survey`, `standard` or `deep` (Sleipnir's depths) |
| `REVIEWER_MODEL` | Sleipnir's | The default model |
| `REVIEWER_ROLE_MODELS` | | `role=model`, comma separated |
| `REVIEWER_BUDGET_USD` | `4` | Model spend cap per review |
| `REVIEWER_TIMEOUT` | `90m` | A review stopped at this length posts what was confirmed |
| `REVIEWER_PASS_ENV` | `HEIMDALLM_API_KEY` | Variables passed to sleipnir (never `GITHUB_*`) |
| `REVIEWER_SANDBOX_IMAGE` | Sleipnir's | The sandbox image |
| `REVIEWER_SANDBOX_NETWORK` | `false` | Let reproducers use the network |
| `REVIEWER_BOT` | `reviewer` | The name in `@reviewer review` |
| `REVIEWER_WORK` | `/srv/reviewer/work` | Where pull requests are cloned |
| `REVIEWER_KEEP` | `false` | Keep each clone and its `swarm.log` after the review |

## Development

```sh
go test ./...
go run ./cmd/reviewer    # with the variables above
```
