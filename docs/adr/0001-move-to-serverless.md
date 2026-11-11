# ADR 0001: Move from a hosted bot to serverless on AWS Lambda

- **Status:** Accepted
- **Date:** 2026-11-11
- **Supersedes:** the hosted design tagged `v0.1-hosted`

## Context

v0.1 is a long-running Go process. It holds a Discord gateway websocket, runs
cron jobs in-process, keeps state in a local SQLite file, and keeps auto-stop
grace timers in memory (persisted to SQLite and resumed on restart). It needs
an always-on host that it cannot stop itself: an Oracle/GCP free VM or a
machine at home.

That works, but for a one-person project it has costs:

- **Ops burden:** someone has to patch, monitor, restart and back up the VM.
  If the host dies, alerting dies with it, silently.
- **Code that exists only because the process is long-lived:** the gateway
  connection, in-memory timers plus their resume-on-restart logic, and the
  in-process cron.
- **A reachable CVE from the gateway client:** GO-2026-6278 in
  `gorilla/websocket`, pulled in by discordgo.

The workload is tiny and bursty: roughly 10k scheduled runs a month plus a
handful of commands. The goals are still the same: see health and cost, get
alerted, stop/start safely, and auto-stop over budget. The priorities are
**lowest cost**, **simplicity**, and **value as an SRE learning project**.

## Decision

Run cloud-control as **AWS Lambda functions (Go)**, triggered by:

1. **Discord HTTP interactions:** slash commands and button presses are
   POSTed to a Lambda Function URL. The gateway websocket goes away.
2. **EventBridge Scheduler:** recurring health (5 min), cost (6 h) and digest
   (09:00 Asia/Manila) runs, plus **one-time schedules** for auto-stop grace
   deadlines.

State moves to **DynamoDB**, secrets to **SSM Parameter Store**, and
logs/metrics to **CloudWatch**. All infrastructure is **Terraform**, deployed
from **GitHub Actions via OIDC** (no long-lived AWS keys).

Commands stay on-demand, and the light schedule stays. Polling only when asked
would break two core goals: "alert me when something goes down" and "auto-stop
when spend crosses budget" both need checks that run without the owner.

```
Discord ──HTTPS (Ed25519-signed)──► Function URL ─► lambda: verify → route
                                                     ├─ fast reply (< 3 s)
                                                     └─ slow ops: defer, async self-invoke,
                                                        edit the reply when done (≤ 15 min)
EventBridge Scheduler ─► lambda {"job":"health"|"cost"|"digest"|"autostop"}
lambda ─► DynamoDB (state) · SSM (secrets) · CloudWatch (logs, EMF metrics)
lambda ─► Discord REST (alerts, edits) · Azure / GCP / Cloudflare / Resend APIs
lambda ─► heartbeat URL (dead man's switch, outside AWS)
```

## Alternatives considered

| Option | Why not |
|---|---|
| **Keep the hosted Go bot** (free VM) | Also $0, but it's a server to babysit, and alerting silently stops if the host dies. Kept as `v0.1-hosted`. |
| **Azure Functions (Flex Consumption)** | Smaller free grant (250k executions / 100k GB-s), which applies only to pay-as-you-go subscriptions. Go needs a custom handler. Cold starts put pressure on Discord's 3 s deadline. Most importantly, it **shares fate with the Azure subscription it watches**: if that subscription is disabled for spending, the watcher goes down with it. |
| **Cloudflare Workers** | Free and fast, but needs a TypeScript rewrite (Go only through WASM), and none of the existing Go code carries over. |
| **On-demand only (no schedule)** | Loses down-alerts and budget auto-stop, the two reasons the project exists. |

## Consequences

**Positive**
- About $0/month: roughly 10k invocations against Lambda's 1M/month always-free
  allowance, ~3k GB-s against 400k, and ~9k scheduler invocations against 14M.
- No host to run. Grace timers become AWS-managed one-time schedules, so
  restart safety comes for free.
- The `gorilla/websocket` CVE disappears along with the gateway.
- Runs on a different cloud from the main thing it watches (Azure).
- Gives a clear SRE story: IaC, keyless CI/CD, SLOs, dead man's switch, runbooks.

**Negative / new risks**
- **The public Function URL** must reject anything without a valid Discord
  Ed25519 signature. Reserved concurrency caps the blast radius of abuse.
- **The 3-second interaction deadline:** anything slow is deferred and
  finished asynchronously.
- **One more cloud account** (AWS) and its credentials. It gets its own
  $1 AWS Budget alarm so the watcher watches itself.
- **The bot must have connected to the Discord gateway at least once** before
  it can post to channels over REST (historically required by Discord). This
  is a one-time step during setup.
- **Testing:** DynamoDB and EventBridge sit behind interfaces with in-memory
  fakes, so `go test` still makes no cloud calls.

## What carries over from v0.1

| v0.1 | Serverless |
|---|---|
| `internal/provider` (+ `fake`) | unchanged |
| `internal/core` (apps, health, cost, budget, control) | unchanged logic; `Store` becomes an interface |
| `internal/autostop` | same state machine; `Timer` → `Scheduler` (create/delete one-time schedule) |
| `internal/store` (SQLite) | → `Store` interface + `memstore` (tests) + `dynamostore` |
| `internal/discord` (gateway) | → HTTP interaction handler + REST notifier; rendering reused |
| `cmd/cloud-control` + cron | → `cmd/lambda` (routes by event) + `cmd/register` (registers slash commands) |
| VM / Docker | → `infra/terraform` |

DynamoDB is a single table with `pk`/`sk` keys. Auto-stop transitions use
conditional writes, replacing SQLite's `UPDATE ... WHERE status = ?`. Health
history expires after 30 days via TTL.

**Safety net for auto-stop:** if creating a one-time schedule fails, the
5-minute health run also executes any pending run whose deadline has passed.
Either path does the pending → executing transition with a conditional write,
so a run can never execute twice.

## Migration milestones

1. **Store interface:** extract it, add `memstore`, move the tests onto it.
2. **Lambda + interactions:** signature verification, PING, `/ping`,
   `/status`; Terraform for the Function URL, IAM and logs; deployed with the
   fake provider.
3. **Scheduled jobs:** health/cost/digest via EventBridge, the REST notifier
   and a heartbeat ping.
4. **Auto-stop:** one-time schedules, the cancel button, the overdue sweep
   (dry run first).
5. **Production hardening:** `dynamostore`, GitHub Actions with OIDC deploy,
   AWS Budget alarm, CloudWatch alarms and dashboard, reserved concurrency.
6. **Azure provider:** Resource Graph, Container Apps health + start/stop,
   Cost Management; least-privilege service principal.
7. **GCP, Cloudflare and Resend providers.**
8. **SRE polish:** SLOs, runbooks, a game day + postmortem, README with a demo GIF.

## SLOs (initial)

- **Freshness:** a health check completes in ≥ 99.5% of 5-minute windows per month.
- **Alert latency:** a Down alert reaches Discord within 10 minutes of the
  first failing check, for ≥ 99% of incidents.
- **Watcher liveness:** the heartbeat is missed for no more than 15 minutes
  before the owner is notified by email (outside AWS).
