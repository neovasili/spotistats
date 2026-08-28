# Spotistats

A personal Spotify listening-history site: seventeen years of plays, captured on a
schedule, rolled up nightly, and served as a static dashboard plus a queryable explorer.

**Live at [spotistats.neovasili.com](https://spotistats.neovasili.com).**

The canonical question it is built to answer well:

> *"How many minutes did I listen to Within Temptation during 2025?"*

| Page | What it does | How it is served |
|---|---|---|
| **Dashboard** (`/`) | Totals, top tracks/artists/albums/genres, listening rhythm, year-by-year history | Pre-rendered JSON on CloudFront — no compute in the request path |
| **Explorer** (`/explore`) | The full catalogue, filterable by period and genre, with play counts and minutes. Every query is a shareable URL. | API Gateway → Lambda → DynamoDB |

## Why it is built this way

Spotify has no listening-history endpoint. `GET /v1/me/player/recently-played` returns at
most 50 plays and reaches back only a few hours, so the only way to have a history is to
poll continuously and never miss a window — and the only way to have a *past* is to import
Spotify's GDPR "Extended Streaming History" export once, by hand.

Everything else follows from that constraint: a capture Lambda on a 30-minute schedule with
overlapping cursors, aggregates maintained incrementally, and a nightly reconcile that
recomputes from the play records so drift cannot accumulate silently.
[`docs/SPECS.md`](docs/SPECS.md) §2 documents the API limits that drive the design; read it
before questioning any decision that follows.

## Architecture

```
Spotify API ──▶ capture (30 min) ──▶ DynamoDB ──▶ rollup (nightly) ──┬──▶ S3/CloudFront  (dashboard JSON)
                                     single table                    └──▶ API Gateway → query λ  (explorer)
MusicBrainz + TheAudioDB ──▶ enrich / resolve ──▶ (artist identity, genres, biographies)
CloudWatch alarms ──▶ SNS ──▶ notify λ ──▶ Slack
```

- **Go 1.26** — six Lambdas (`cmd/{capture,rollup,enrich,resolve,query,notify}`) and an
  operator CLI (`cmd/spotistats`).
- **AWS CDK v2, also in Go** (`infra/`) — one regional stack in `eu-west-1`, one global stack
  in `us-east-1` for the CloudFront certificate and the billing budget.
- **DynamoDB single table** — plays, dimensions, aggregates and leaderboards share one table;
  key design in `internal/store/keys.go`.
- **React 19 + TypeScript + Vite** (`web/`) — static bundle, no runtime framework server.

Target cost is under $2/month, which is itself a design constraint rather than an outcome.

## Running it locally

The local flow needs Docker and no AWS account at all: DynamoDB Local via testcontainers,
synthetic seed data, and the real frontend against a real backend.

```sh
make dev-all      # container + table + synthetic data + rendered snapshots
make serve        # the API, on :8787
make web-dev      # the frontend, on :5173, proxied to the API
```

`make help` lists every target.

## Running your own

This is single-user by design — one Spotify account, one deployment, no multi-tenancy or
viewer auth (the site is public on purpose). It is nonetheless fully reproducible for your
own account.

[`docs/PREREQUISITES.md`](docs/PREREQUISITES.md) is the manual runbook: everything that
cannot be automated because it involves consent screens, email confirmations, DNS, and
secrets that must never enter git. **Start with step 1** — requesting your Extended
Streaming History from Spotify has up to 30 days of latency.

Two things are deliberately kept out of `cdk.json` because they are account-specific:

| Value | Where it comes from |
|---|---|
| AWS account ID | Resolved by the CDK CLI from your active credentials |
| Route 53 hosted zone ID | `SPOTISTATS_HOSTED_ZONE_ID` (see `make dev-env`) or `cdk -c hostedZoneId=...` |

No secret is ever stored in a CloudFormation template. Spotify credentials, the TheAudioDB
key and the Slack webhook are hand-created SSM SecureStrings under `/spotistats/spotify`,
read at runtime; `internal/config` is the single place in the codebase that calls
`os.Getenv`, and `Config.Redacted()` has a test that enumerates fields so a newly added
credential cannot be logged unredacted.

## Tests

```sh
make test-short   # pure Go: no Docker, no network, no AWS
make test         # adds the DynamoDB Local integration suite
make fuzz         # 30s fuzz on the aggregate engine
make ci           # everything CI runs
```

No test needs real credentials. Every external API is faked (`internal/*/[*]test/` plus JSON
fixtures under each `testdata/`), and `test-short` failing to be pure — needing a container,
the network, or AWS — is itself treated as a regression.

The browser smoke suite (`make smoke`) defaults to your local dev server; point it elsewhere
with `SMOKE_BASE_URL`.

## Documentation

- [`docs/SPECS.md`](docs/SPECS.md) — design and architecture, including the Spotify API
  constraints that drive it and the correctness model for aggregates.
- [`docs/PREREQUISITES.md`](docs/PREREQUISITES.md) — the manual setup runbook.

## Licence

[MIT](LICENSE).
