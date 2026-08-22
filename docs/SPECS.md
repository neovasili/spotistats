# Spotistats — Design & Architecture Specification

**Status:** Draft v1
**Last updated:** 2026-08-22
**Owner:** @neovasili

---

## 1. Overview

Spotistats is a personal, publicly-readable static website that surfaces the owner's
Spotify listening statistics. It has two faces:

| Page | Purpose | Data path |
|---|---|---|
| **Dashboard** (`/`) | At-a-glance highlights: totals, top tracks/artists/albums/genres, listening rhythm | Pre-rendered JSON on CloudFront (no compute) |
| **Explorer** (`/explore`) | Full catalogue of everything listened to, with play counts, minutes, filtering, and ad-hoc grouped queries | Query API (API Gateway → Lambda → DynamoDB) |

The canonical target query the Explorer must answer well:

> *"How many minutes did I listen to Within Temptation during 2025?"*

Data is captured from the Spotify Web API on a schedule, stored in DynamoDB, and
rolled up nightly. Deep history is loaded once from Spotify's GDPR data export via a
local CLI.

### 1.1 Goals

- Zero-maintenance: runs unattended, self-heals aggregate drift.
- Cheap: target < $2/month steady state.
- Fast: dashboard is static JSON on CDN; p95 first contentful paint < 1s.
- Correct: play counts and minutes must not double-count or silently drift.
- Single environment (`production`), single region, single user's data.

### 1.2 Non-goals

- Multi-user / multi-tenant. Auth for viewers (the site is public by design).
- Real-time "now playing" (would need polling far more aggressive than is justified).
- Audio-feature analysis (danceability, energy, valence) — **not available**, see §2.3.
- Recommendations or discovery features — **not available**, see §2.3.
- WAF (explicitly out of scope; see §10.3 for the compensating controls).

---

## 2. Hard constraints from the Spotify Web API

> **This section drives the entire architecture.** Every constraint below was
> verified against Spotify's current developer documentation. Read this before
> questioning any design decision that follows.

### 2.1 There is no listening-history endpoint

`GET /v1/me/player/recently-played` is the *only* source of play events, and it is
severely limited:

- **`limit` maximum is 50.** No exceptions.
- Cursors are `before` / `after`, both **Unix timestamps in milliseconds**, and only
  one may be supplied per call.
- In practice Spotify retains only **the most recent ~50 plays**. You cannot page
  backwards into last month. `before` lets you walk *within* that retained window,
  not beyond it.
- **Podcast episodes are excluded** — the docs state it outright: *"Currently doesn't
  support podcast episodes."* Spotistats is therefore music-only, and this should be
  stated in the UI so the totals aren't misread.
- Requires scope `user-read-recently-played`.

**Consequence — and the first correction to the original brief:** a *nightly*
capture job will lose data. Fifty tracks is roughly three hours of listening. Any day
with heavier listening than that silently drops plays, permanently, with no way to
recover them from the API.

**Design response:** the capture job runs **every 2 hours** (12×/day → ~600 plays/day
of headroom). The *nightly* job is retained but repurposed: it does rollups,
dimension refresh, snapshot rendering, and reconciliation. Capture frequency and
rollup frequency are decoupled. Cost impact is negligible (12 Lambda invocations of
~2s per day). If the gap detector (§4.1) still reports saturation, drop to hourly.

### 2.2 `recently-played` does not return listening duration

The endpoint tells you *that* a track was played at time T. It does **not** return
how much of it was played. Spotify counts a play once roughly 30 seconds are
consumed, but the actual `ms_played` is unavailable.

The GDPR export (§2.4) *does* include exact `ms_played`.

**Consequence:** the two ingest sources have different fidelity. Minutes derived from
API-captured plays are **estimates**; minutes from the export are **exact**. Silently
mixing them would produce a number that is wrong in a way nobody can detect later.

**Design response:** every play row carries `source` (`api` | `export`) and
`msEstimated` (bool). For `source=api`, `msPlayed := track.duration_ms`, which
over-counts skips. The API exposes both an exact and an estimated minutes figure, and
the UI labels any total that includes estimated data. See §5.2 and §6.4.

### 2.3 Endpoints unavailable to new apps (post-2024-11-27)

Spotify restricted these for apps registered on or after 2024-11-27, and for existing
apps still in development mode:

| Restricted | Impact on Spotistats |
|---|---|
| Audio Features | No danceability / energy / valence / tempo charts |
| Audio Analysis | No structural analysis |
| Recommendations | No "you might like" |
| Related Artists | No artist-similarity graph |
| Get Featured Playlists | Not needed |
| Get Category's Playlists | Not needed |
| 30-second preview URLs (multi-get) | No inline audio previews |
| Algorithmic / editorial playlists | Not needed |

Spotistats will be a new app, so **all of the above are off the table.** Do not spec
features that depend on them.

Still available and used: `recently-played`, `me/top/{artists,tracks}`,
`tracks`, `artists`, `albums` (multi-get for metadata enrichment).

> **Never send `market` on metadata enrichment.** With a market set, Spotify applies
> *track relinking* and returns a **different track ID than the one requested**, so the
> same song silently accumulates statistics under two identities. All multi-get calls
> omit it, and a test asserts no request carries it. **Genres come from
the artist object** (`GET /v1/artists`) — that is the only taxonomy available, and it
is per-artist, never per-track.

### 2.4 Deep history requires the GDPR export

Full history back to account creation is only obtainable by requesting **Extended
Streaming History** from Spotify's privacy settings page. Properties:

- Free, self-service, but **delivery takes up to 30 days**.
- Arrives as a zip of JSON files, each ~12 MB, plus a PDF describing the schema.
- Per-play fields include `ts`, `ms_played`, `master_metadata_track_name`,
  `master_metadata_album_artist_name`, `master_metadata_album_album_name`,
  `spotify_track_uri`, `platform`, `conn_country`, `reason_start`, `reason_end`,
  `shuffle`, `skipped`, `offline`.
- The plain "account data" request returns only ~30 days — **request the extended
  version**, they are separate requests from the same page.

**Consequence:** "history backfill" is not an API operation. It is an offline import
of a file the user must request by hand, well in advance.

**Design response:** the local CLI (§8) imports the export. Because the export is the
long-lead item, requesting it is **step 1** of the prerequisites document.

### 2.5 Rate limits & quota

- Rolling **30-second** window. Exceeding it returns **429** with a `Retry-After`
  header (seconds).
- New apps sit in **development mode**: lower quota, max 25 users. Extended quota mode
  requires an application review aimed at production multi-user apps and will not be
  granted for a personal dashboard.

**Design response:** stay in development mode — it is sufficient for one user. All
Spotify calls go through a client that respects `Retry-After` exactly, uses capped
exponential backoff with jitter for 5xx, and batches metadata lookups at the API
maximums (tracks 50, artists 50, albums 20) to minimise call volume.

### 2.6 Token lifecycle

- Authorization Code flow. Access tokens live **1 hour**.
- Refresh tokens do not expire but can be revoked, and Spotify **may return a new
  refresh token** on refresh.

**Design response:** the refresh token is stored in SSM Parameter Store as a
`SecureString`. The Lambda writes back a rotated token whenever one is returned; if
it does not, the existing value stands. A `POST` failure on rotation is a hard alarm —
losing a rotated refresh token means re-running the manual auth flow.

---

## 3. Architecture

```
                            ┌──────────────────────┐
   Spotify Web API ◀────────┤  capture-lambda      │◀── EventBridge (every 2h)
                            │  (Go, arm64)         │
                            └──────────┬───────────┘
                                       │ conditional writes
                            ┌──────────▼───────────┐
   Spotify Web API ◀────────┤   DynamoDB           │
   (metadata enrich)        │   spotistats (1 tbl) │
                            └──────────┬───────────┘
                                       │ read
                            ┌──────────▼───────────┐
                            │  rollup-lambda       │◀── EventBridge (nightly 03:15 UTC)
                            │  reconcile + render  │
                            └──────────┬───────────┘
                                       │ writes dashboard.json, catalog.json
                            ┌──────────▼───────────┐
                            │   S3 (private)       │
                            │   web/ + data/       │
                            └──────────┬───────────┘
                                       │ OAC
   Browser ──── CloudFront ────────────┤
        stats.example.com              │
                    │                  └── /api/* ──▶ API Gateway (HTTP API)
                    │                                        │
                    │                              ┌─────────▼─────────┐
                    │                              │  query-lambda     │
                    │                              │  (Go, arm64)      │
                    │                              └─────────┬─────────┘
                    │                                        │
                    └────────────────────────────────────────┴──▶ DynamoDB

   Local machine ──▶ (AWS creds) ──▶ DynamoDB          [spotistats CLI: import/reconcile]
```

### 3.1 Key architectural decisions

| Decision | Rationale |
|---|---|
| **Single DynamoDB table** | All access patterns are known and enumerable (§5.1). One table = one set of IAM/metrics/backup concerns. |
| **Dashboard served as static JSON, not via the API** | The dashboard is identical for every visitor and changes once a night. Rendering it nightly to S3 makes the landing page pure CDN: no Lambda cold start, no DynamoDB read cost, no throttling exposure, and it survives a Lambda outage. |
| **API only for the Explorer** | Arbitrary grouping and filtering genuinely needs compute. This is where API Gateway + Lambda earn their place. |
| **CloudFront fronts both origins** | `/api/*` → API Gateway, everything else → S3. Same-origin means **no CORS** and lets API responses cache at the edge. |
| **Aggregates maintained at write time** | The target query ("minutes of Within Temptation in 2025") becomes a single `GetItem` instead of a scan. See §5.3. |
| **Nightly reconciliation** | Write-time counters can drift if a Lambda dies mid-update. Raw play events are the source of truth; aggregates are a cache that gets rebuilt. See §4.3. |
| **Backfill runs locally, not in Lambda** | The import is a one-off, needs a multi-GB local file, and takes far longer than the 15-minute Lambda ceiling. Local execution also avoids exposing a privileged bulk-write endpoint to the internet. |
| **Everything in `us-east-1`** | CloudFront requires its ACM certificate in `us-east-1`. Deploying the whole stack there avoids cross-region certificate plumbing for zero practical downside on a personal project. |

---

## 4. Data ingestion

### 4.1 Capture pipeline (`capture-lambda`, every 2 hours)

1. Read `STATE / POLL_CURSOR` → `lastPlayedAt` (Unix ms).
2. Fetch access token: read refresh token from SSM, POST to `/api/token`. Persist a
   rotated refresh token if one is returned.
3. `GET /v1/me/player/recently-played?limit=50&after={lastPlayedAt}`.
4. **Gap detection:** if the response contains exactly 50 items, listening may have
   exceeded the window. Write a `GAP` marker item recording the interval and emit the
   `PlaysGapDetected` metric at value 1. This is the signal to increase capture
   frequency.
   - **Open question to settle empirically.** Spotify does not document whether `after`
     returns the *oldest* or the *newest* matching items. If it returns the newest, a
     saturated page means plays were definitely lost, not merely at risk. Every run
     therefore logs the requested cursor, both echoed cursors, and the observed
     min/max `played_at`. Until this is answered with real data there is deliberately
     no auto-paginating iterator.
5. **Resolve artist genres — before recording anything.** Batch-read the artist rows for
   every artist on the page; fetch and persist any that are unknown or stale via
   `GET /v1/artists`.
   - Spotify returns positionally-aligned `null` for IDs it cannot resolve (removed,
     relinked, invalid). Those get a **tombstone** `META` row with `missing: true`.
     Without it the enrichment pass would re-request the same dead IDs on every run
     forever, burning quota that development mode cannot spare.
   - **This step must precede step 6, and an earlier draft of this spec had it after.**
     Genres exist only on the artist object, so recording a play before its artist row
     exists means the play contributes *zero* genre deltas — silently, and permanently
     until a reconcile. Artists are also the only entity needing an API call during
     capture.
   - **If artist resolution fails, do not abort the run.** Record the plays with whatever
     genres are known and mark the run degraded. The reasoning: the endpoint retains only
     ~50 plays, so refusing to record them risks the window rolling and losing them
     *permanently*, whereas incomplete genre aggregates are **recoverable** — §4.3's
     reconcile recomputes them from the raw plays once the artist rows exist. Prefer the
     recoverable failure.
6. For each play, oldest → newest:
   - `PutItem` the play row with `ConditionExpression: attribute_not_exists(PK)`.
     A `ConditionalCheckFailedException` means "already ingested" — skip it, do not
     touch aggregates. This makes the whole pipeline idempotent and safe to re-run.
   - On successful insert, apply the aggregate deltas (§5.3) via `UpdateItem ... ADD`.
7. **Persist track and album metadata from the payload already in hand.** `recently-played`
   embeds full track objects and simplified album objects, so this costs **no extra API
   calls** — only artists ever need one. Rows older than 30 days are refreshed
   opportunistically. A failure here is logged and ignored: metadata is display-only, never
   feeds an aggregate, and must not discard a successful ingest.
8. Advance `POLL_CURSOR` to the newest `played_at` **only after** all writes succeed. It
   only ever moves forward — Spotify's ordering guarantees are unstated, so a page whose
   newest item predates the stored cursor must not rewind it. Failing before this point
   means the next run re-reads the same window, which is harmless because step 6 is
   idempotent.

Ordering matters, in three places: genres are resolved before the write, the play insert
gates the aggregate update, and the cursor advances last. The failure mode is therefore
always "redo work" or "recoverable incompleteness", never "lose a play" or "double-count".

### 4.2 Backfill pipeline (local CLI, one-off)

```
spotistats backfill import --path ./my_spotify_data.zip [--dry-run]
```

1. Stream-parse each `Streaming_History_Audio_*.json` entry.
2. **Filter:** drop `ms_played < 30000` by default (`--min-ms` to override) so the
   definition of a "play" matches the API's ~30s threshold. Without this, API-era and
   export-era play counts are not comparable. Drop rows with a null
   `spotify_track_uri` (podcasts, local files) and report the count.
3. Write play rows with `source=export`, `msEstimated=false`, exact `msPlayed`.
4. **Aggregate locally, then write once.** Naively streaming `ADD` increments for
   ~100k plays would cost millions of write units. The CLI accumulates the full
   aggregate map in memory and issues one `PutItem` per aggregate row at the end —
   thousands of writes instead of millions.
5. Enrich dimensions: collect distinct track URIs, resolve via `GET /v1/tracks?ids=`
   in batches of 50, then artists and albums. This is the slow part; it is resumable
   via a `STATE / ENRICH_CURSOR` row.
6. Write `STATE / INGEST#{yyyy-mm}` markers claiming each covered month for the
   `export` source.

**Source precedence — avoiding double-counted overlap.** The export and the API
describe the same plays for any overlapping period, but with different timestamps
(the export's `ts` is the stream *end*; the API's `played_at` differs by seconds).
Conditional writes cannot dedupe them because the keys differ. Rule:

> For any month claimed by an `INGEST#{month}` marker with `source=export`, the export
> is authoritative. The importer **deletes** `source=api` play rows in that month
> before writing, and the month's aggregates are recomputed from scratch.

The CLI reports exactly how many API rows it superseded.

### 4.3 Nightly job (`rollup-lambda`, 03:15 UTC)

1. **Reconcile.** Recompute aggregates from raw play rows for the trailing 45 days and
   compare to stored counters. Any mismatch is corrected and emitted as
   `AggregateDrift`. A non-zero value means a capture run died between the play insert
   and the aggregate update — expected to be rare, but this makes it self-healing
   rather than permanently wrong. Full-history reconciliation is available on demand
   via `spotistats reconcile --all`.
2. **Refresh materialised leaderboards** (`TOP#*`) for the affected periods.
3. **Refresh top-items** from `me/top/{artists,tracks}` for all three
   `time_range` values (`short_term` ≈ 4 weeks, `medium_term` ≈ 6 months,
   `long_term` ≈ 1 year). These are Spotify's own rankings and are stored alongside
   Spotistats' computed ones — they will differ, and the UI labels which is which.
4. **Render snapshots** to S3: `data/dashboard.json`, `data/catalog.json`,
   `data/meta.json`. Written with `Cache-Control: public, max-age=300, s-maxage=86400`
   followed by a targeted CloudFront invalidation of `/data/*`.

---

## 5. Data model

Single DynamoDB table `spotistats`, on-demand billing, PITR enabled.

- Keys: `PK` (S, HASH), `SK` (S, RANGE)
- `GSI1`: `GSI1PK` (S), `GSI1SK` (S) — projection `INCLUDE [msPlayed, source, msEstimated, trackId]`.
  Not sparse: every play row sets `GSI1PK`, so the index is a full replica of the play
  data. That is the right trade for access pattern 6 and costs ~1 extra WCU per play,
  but reading any attribute *outside* the projection yields a zero value rather than an
  error — a silent-bug source with a dedicated regression test.

### 5.1 Access patterns

| # | Pattern | Implementation |
|---|---|---|
| 1 | Minutes/plays for entity X in period P | `GetItem AGG#{DIM}#{P} / {id}` — **O(1)** |
| 2 | Minutes/plays for entity X across a month range | `BatchGetItem` over the range's monthly PKs |
| 3 | Top N tracks/artists/albums/genres for period P | `GetItem TOP#{DIM}#{P} / V1` (materialised) |
| 4 | Full ranked list for period P, paginated | `Query AGG#{DIM}#{P}`, sort in Lambda |
| 5 | Every play in a time range | `Query PLAY#{yyyy-mm}` per month in range |
| 6 | Every play of one track, chronologically | `Query GSI1 where GSI1PK = TRACK#{id}` |
| 7 | Daily listening totals (calendar heatmap) | `Query AGG#TOTAL#{yyyy}` prefix on day SKs |
| 8 | Hour-of-day / day-of-week rhythm | `GetItem HIST#{P} / HOUR` \| `/ DOW` |
| 9 | Name → ID resolution (search) | Client-side over `catalog.json` (§6.3) |
| 10 | Track/artist/album metadata | `GetItem {DIM}#{id} / META` |
| 11 | Ingest state, cursors, gap markers | `Query PK = STATE` |

### 5.2 Item shapes

**Play event** — immutable fact, the source of truth.
```
PK        PLAY#2025-03
SK        2025-03-14T21:04:33.123Z#4uLU6hMCjMI75M1A2tKUQC
GSI1PK    TRACK#4uLU6hMCjMI75M1A2tKUQC
GSI1SK    2025-03-14T21:04:33.123Z
type      play
trackId   4uLU6hMCjMI75M1A2tKUQC
artistIds ["1n9rbSlgWTMSaqvQdgTP1P"]
albumId   1DFixLWuPkv3KT3TnV35m3
msPlayed  231000
source    export | api
msEstimated  false | true
platform  ios          (export only)
country   ES           (export only)
shuffle   false        (export only)
skipped   false        (export only)
reasonEnd trackdone    (export only)
```
Month-bucketed PK keeps partitions bounded (~3k items/month worst case) and makes range
queries a small, predictable number of partition reads. **The partition is the UTC month**
(see §14 decision 0b), so a local calendar month spans two partitions; one helper owns that
fan-out.

**Timestamp format is pinned to `2006-01-02T15:04:05.000Z07:00`** — fixed three-digit
fractional width, always UTC. This is load-bearing, not cosmetic: sort keys and the
`firstPlayedAt`/`lastPlayedAt` attributes are compared **lexically** by DynamoDB, and Go's
`time.RFC3339Nano` strips trailing zeros, so `.120` renders as `.12Z` and then sorts *after*
`.123Z`, inverting chronological order. `time.RFC3339` and `time.RFC3339Nano` are banned
repo-wide by lint.

**Dimensions**
```
PK  TRACK#{id}    SK  META    name, durationMs, albumId, artistIds, popularity,
                                explicit, isrc, uri, refreshedAt
PK  ARTIST#{id}   SK  META    name, genres[], popularity, followers, imageUrl, refreshedAt
PK  ALBUM#{id}    SK  META    name, releaseDate, releaseDatePrecision, imageUrl,
                                totalTracks, artistIds, refreshedAt
```

**Aggregate** — the query engine.
```
PK  AGG#{DIM}#{PERIOD}     DIM    ∈ TRACK | ARTIST | ALBUM | GENRE | TOTAL
SK  {entityId}             PERIOD ∈ ALL | 2025 | 2025-03 | 2025-03-14
    plays, playsExact, msPlayed, msPlayedExact, firstPlayedAt, lastPlayedAt
    dim, period, entityId          (denormalised copies of what the keys encode)
```
`msPlayed` includes estimated data; `msPlayedExact` counts only `source=export` rows, and
is a **subset** of `msPlayed` rather than a parallel total — so
`estimatedRatio = 1 − exact/total`. `playsExact` is the same split applied to the count: it
costs one extra `ADD` clause in an `UpdateItem` already being issued, and answers "does this
month contain api-sourced rows?" in a single read, which §4.2's export-precedence rule
needs. Reporting both is what makes §2.2 honest instead of hidden.

**Key layout, including one deliberate exception.** `SK` is the entity ID, except that
`DIM=TOTAL` rows have no entity and use the literal `ALL`, and `TOTAL` at *day* granularity
is folded into its year's partition:

```
TOTAL + day    →  PK "AGG#TOTAL#{yyyy}",     SK "{yyyy-mm-dd}"
TOTAL + year   →  PK "AGG#TOTAL#{yyyy}",     SK "ALL"
TOTAL + month  →  PK "AGG#TOTAL#{yyyy-mm}",  SK "ALL"
TOTAL + all    →  PK "AGG#TOTAL#ALL",        SK "ALL"
anything else  →  PK "AGG#{DIM}#{PERIOD}",   SK "{entityId}"
```

The fold makes the calendar heatmap one `Query` over a year partition with
`begins_with(SK, "2025-")` instead of 365 `GetItem`s. Monthly totals live in their own
partition, so that prefix matches day rows only. Exactly one function pair
(`AggKey.PK()/SK()`) knows about the exception.

**`firstPlayedAt` / `lastPlayedAt` are best-effort at write time.** DynamoDB has no
MIN/MAX, so `firstPlayedAt` uses `if_not_exists` and `lastPlayedAt` is set unconditionally —
an out-of-order write (a re-run, or backfill) can move `lastPlayedAt` backwards. The
**counters, which is what every query reports, are always exact**; the nightly reconcile
recomputes the two bounds from raw plays. Merging deltas in memory before writing computes
true min/max for the batch case.

**`AGG#TOTAL#ALL / ALL` is the hottest key in the design, by choice.** Every play writes it.
At ~100 plays/day it is four orders of magnitude below the per-partition write ceiling, and
§4.2's in-memory pre-aggregation keeps backfill off it entirely. Do not "fix" it.

**Genre counting.** Genres live on the *artist* object, never the track, so a play's genres
are the **deduplicated union across its artists** — if two artists on one play are both
tagged `gothic metal`, that is one genre-play, not two. Normalisation lowercases and
collapses whitespace but does not slugify, since the normalised form is both the sort key
and the display string.

> **Genres are a many-to-many labelling, and this has a consequence that is easy to get
> wrong.** A play whose artists carry three genres contributes one play to *each* of three
> genre rows, so `Σ GENRE.plays` can **exceed** the overall total. Meanwhile plays by
> genre-less artists — the common case, most artists carry none — contribute nothing,
> pulling it down. **There is no ordering bound between the genre sum and the total in
> either direction**, so `Other = TOTAL − Σ genres` is not a valid derivation and can go
> negative. The quantity that *is* bounded by the total is "plays carrying at least one
> genre". See §7.3 for what this means for the chart.

Granularity is deliberately asymmetric to control write amplification:

| DIM | ALL | year | month | day |
|---|---|---|---|---|
| TOTAL | ✓ | ✓ | ✓ | ✓ |
| TRACK / ARTIST / ALBUM / GENRE | ✓ | ✓ | ✓ | — |

Day-level rows exist only for `TOTAL` (the calendar heatmap needs them; nothing needs
per-track-per-day). A single play with 2 artists and 3 genres costs
`4 (TOTAL) + 3 (track) + 6 (artists) + 3 (album) + 9 (genres) = 25` updates — trivial
at ~100 plays/day, and bypassed entirely during backfill by §4.2 step 4.

**Materialised leaderboard**
```
PK  TOP#{DIM}#{PERIOD}   SK  V1
    items: [{id, name, plays, msPlayed, imageUrl}, …100]
    computedAt
```
One `GetItem` per dashboard widget instead of a partition query plus an in-Lambda sort.

**Histogram**
```
PK  HIST#{PERIOD}   SK  HOUR   h0…h23 (plays, msPlayed)
PK  HIST#{PERIOD}   SK  DOW    d0…d6
```
Hour is bucketed in the **listener's local timezone** (`Europe/Madrid`), configured as
an env var, not UTC — a "listening by hour of day" chart in UTC is meaningless.

**State**
```
PK  STATE   SK  POLL_CURSOR        lastPlayedAt, lastRunAt, lastStatus
PK  STATE   SK  ENRICH_CURSOR      resumable backfill enrichment position
PK  STATE   SK  INGEST#{yyyy-mm}   source (export|api), importedAt, playCount
PK  STATE   SK  GAP#{ts}           windowStart, windowEnd, itemsReturned
```

### 5.3 Worked example — the canonical query

> *"Minutes listening to Within Temptation songs during 2025"*

1. Client resolves `"Within Temptation"` → `ARTIST#1n9rbSlgWTMSaqvQdgTP1P` from the
   locally-cached `catalog.json`. No API call.
2. `GET /api/v1/stats?dim=artist&id=1n9rbSlgWTMSaqvQdgTP1P&period=2025`
3. Lambda issues **one** `GetItem`: `PK=AGG#ARTIST#2025, SK=1n9rbSlgWTMSaqvQdgTP1P`.
4. Response: `{plays, msPlayed, msPlayedExact, firstPlayedAt, lastPlayedAt}`.

Single-digit-millisecond, ~0.5 RCU. A non-aligned range (`2025-03` → `2025-08`) becomes
one `BatchGetItem` over six monthly rows.

---

## 6. API

Base path `/api/v1`, served through CloudFront. HTTP API (API Gateway v2) — cheaper
and lower-latency than REST API, and none of the REST-only features are needed. All
routes are `GET`, unauthenticated, read-only.

### 6.1 Endpoints

| Route | Query params | Returns |
|---|---|---|
| `/stats` | `dim`, `id`, `period` \| `from`+`to`, `metric` | Single-entity aggregate (§5.3) |
| `/top` | `dim`, `period`, `limit`, `metric` | Ranked leaderboard |
| `/list` | `dim`, `period`, `sort`, `order`, `limit`, `cursor`, `q` | Paginated full catalogue |
| `/plays` | `trackId` \| `from`+`to`, `limit`, `cursor` | Raw play events |
| `/timeline` | `dim`, `id`, `from`, `to`, `bucket` | Per-bucket series for charting |
| `/meta` | — | Coverage window, last update, row counts, estimated-data ratio |

`dim` ∈ `track|artist|album|genre`. `period` is `ALL`, `YYYY`, `YYYY-MM`, or
`YYYY-MM-DD`. `metric` ∈ `plays|ms|msExact`. `bucket` ∈ `day|month|year`.

### 6.2 Conventions

- Pagination is opaque-cursor: base64 of the DynamoDB `LastEvaluatedKey`. No offsets.
- `limit` defaults to 50, hard-capped at 500.
- Errors: `{"error": {"code": "INVALID_PERIOD", "message": "…"}}` with a 4xx status.
  Validation is strict — unknown query params are rejected rather than ignored, so
  typos surface immediately instead of silently returning wrong data.
- `Cache-Control: public, max-age=60, s-maxage=3600`. Data changes once nightly, so
  edge-caching for an hour is free correctness.
- Responses are gzip/brotli compressed at CloudFront.

### 6.3 Search without a search engine

DynamoDB has no text search, and OpenSearch would cost more than the rest of the stack
combined. Instead the nightly job renders `data/catalog.json`:

```json
{"generatedAt":"…","artists":[["1n9rb…","Within Temptation"]],"tracks":[…],"albums":[…]}
```

A few thousand artists and tens of thousands of tracks compress to a few hundred KB
brotli'd. The frontend fetches it once, caches it in IndexedDB keyed on
`generatedAt`, and does fuzzy matching client-side. Instant, zero-cost, offline-capable.

### 6.4 Honest metrics contract

Any response carrying `msPlayed` also carries:

```json
{"msPlayed": 8420000, "msPlayedExact": 6110000, "estimatedRatio": 0.27}
```

The UI must render an indicator whenever `estimatedRatio > 0`. This is the API-level
enforcement of §2.2 — it makes the fidelity difference impossible to accidentally
present as exact.

---

## 7. Frontend

### 7.1 Stack

- **Vite** + **React 19** + **TypeScript** (strict).
- **TanStack Router** (typed routes) + **TanStack Query** (fetch/cache/retry).
- **Tailwind CSS** for layout; charts hand-built on **D3 scales + SVG** rather than a
  charting library, because the spec below pins exact mark geometry that most
  libraries fight.
- **Vitest** + Testing Library; **Playwright** for a smoke suite.
- Static output, no SSR. Hash-named assets `immutable`; `index.html` `no-cache`.

### 7.2 Pages

**Dashboard `/`** — reads `data/dashboard.json` only.
- Hero figure: total listening time, all-time.
- KPI row: total plays · distinct tracks · distinct artists · current daily streak.
- Top artists, top tracks, top albums (top 10 each, ranked bars).
- Genre mix.
- Calendar heatmap, trailing 12 months.
- Listening rhythm: by hour of day, by day of week.
- Footer: coverage window, last-updated, estimated-data disclosure, music-only note.

**Explorer `/explore`** — query API.
- Filter row (single row, above the charts): entity-type toggle, period picker,
  free-text search, metric selector, sort.
- Virtualised table: rank · name · artist · plays · minutes · first · last played.
- Query builder producing the §5.3 call, with the resolved query echoed in prose
  ("Within Temptation · 2025 · minutes") and a shareable deep-linked URL.
- Drill-down: row → per-track play timeline.
- Every view has a **table** representation and a CSV export.

### 7.3 Visualization spec

Forms are chosen by the data's job, not by variety. Almost every question on this
dashboard is *"compare magnitude, low → high"*, which means **sequential single-hue,
not categorical**. Categorical colour is reserved for the rare case where distinct
series are genuinely the subject.

| Widget | Form | Colour job |
|---|---|---|
| Total listening time | Hero figure, ≥48px | text tokens only |
| Plays / tracks / artists / streak | KPI row of stat tiles | text tokens only |
| Top artists / tracks / albums | Horizontal ranked bars | sequential blue |
| Genre mix | Horizontal **ranked** bar (see note below) | sequential blue |
| Daily activity, 12 months | Calendar heatmap | sequential blue |
| Hour of day / day of week | Column chart | sequential blue |
| Minutes over time for a query | Line (single series) | 1 categorical slot |
| Two entities compared | Multi-line, ≤3 series | categorical slots 1–3 |

> **Genre mix is a ranked bar, not a stacked bar.** A stacked bar is a part-to-whole form,
> and genre data cannot be part-to-whole: a track belongs to several genres at once, so the
> segments do not sum to the total and no honest "100%" exists (§5.2). Use a ranked
> horizontal bar of minutes per genre and state the caveat in the UI — a track can count
> under more than one genre. Report genre *coverage* (the share of listening whose artists
> carry any genre at all) as a separate figure rather than as a synthetic "Other" segment.

**Palette** (validated — see §7.4):

```
sequential blue   #cde2fb #b7d3f6 #9ec5f4 #86b6ef #6da7ec #5598e7 #3987e5
                  #2a78d6 #256abf #1c5cab #184f95 #104281 #0d366b
categorical       slot 1 #2a78d6  slot 2 #eb6834  slot 3 #1baf7a   (light)
                  slot 1 #3987e5  slot 2 #d95926  slot 3 #199e70   (dark)
diverging         blue ↔ red, neutral gray midpoint (#f0efec / #383835)
surfaces          light #fcfcfb   dark #1a1a19
```

Non-negotiables carried into implementation:

- **Never a dual-axis chart.** Plays and minutes are different scales → two charts or
  index both to a common base.
- Colour follows the **entity**, never its rank — filtering the list must not repaint
  the survivors.
- Categorical hues are assigned in fixed slot order and never cycled. A 4th series
  folds into "Other" or facets into small multiples; a generated hue is never an option.
- Marks: 2px lines, ≥8px hover markers, 4px rounded bar ends anchored to the baseline,
  2px surface gap between adjacent/stacked fills, recessive grid and axes.
- Direct-label selectively — never a number on every point.
- Every chart with ≥2 series has a legend; ≤4 series are also direct-labelled, so
  identity is never conveyed by colour alone.
- Hover layer by default: crosshair + tooltip on line/area, per-mark tooltip on
  bar/column/cell. Only bare stat tiles opt out.
- Dark mode uses the **selected** dark steps above — not a programmatic inversion.
- A texture fill is available for CVD, print, and `forced-colors`.

### 7.4 Palette validation

The palette above was checked with the dataviz validator rather than by eye. Result for
the three categorical slots under the strictest `--pairs all` mode:

| Mode | Lightness | Chroma | CVD ΔE | Normal-vision ΔE | Contrast |
|---|---|---|---|---|---|
| light | PASS | PASS | PASS 9.2 (deutan) | PASS 24.0 | **WARN** — aqua 2.74:1 |
| dark | PASS | PASS | PASS 9.4 (deutan) | PASS 20.9 | PASS |

The light-mode contrast WARN on aqua (`#1baf7a`, 2.74:1 vs surface) is **not
dismissable**: it obligates the relief rule — visible direct labels or the table view
wherever slot 3 is used on the light surface. §7.2 already mandates a table view on
every chart, which satisfies it; direct labels on the genre stacked bar satisfy it
again. Re-run the validator if any hex changes.

---

## 8. Local CLI (`cmd/spotistats`)

A single Go binary using the operator's own AWS credentials directly against DynamoDB —
no privileged HTTP endpoint exists, so there is nothing internet-facing to abuse.

| Command | Purpose |
|---|---|
| `auth login` | **Built.** Runs the one-time OAuth flow: loopback listener on `http://127.0.0.1:8888/callback`, random `state` validated on the callback, code exchanged, granted scopes checked, refresh token written. |
| `auth status` | **Built.** Exercises a real token refresh and calls `recently-played` — a stored token Spotify has revoked is indistinguishable from a good one until it is used. |
| `config` | **Built.** Prints the resolved configuration with secrets redacted. |
| `backfill import --path <zip>` | Imports the GDPR export (§4.2). `--dry-run`, `--min-ms`, `--from`, `--to`. |
| `backfill enrich` | Resumes metadata enrichment for tracks/artists/albums lacking a `META` row. |
| `reconcile [--all|--from|--to]` | Recomputes aggregates from raw plays and reports drift. |
| `poll` | **Built.** Runs the capture pipeline (§4.1). `--dry-run` reports what would be ingested without writing; `--limit` overrides the page size. |
| `render` | Regenerates and uploads the S3 snapshots on demand. |
| `export --out <dir>` | Dumps all raw plays to JSONL as a personal backup. |

Every mutating command supports `--dry-run` and prints a summary diff before writing.

### 8.1 Running without AWS

The refresh token normally lives in SSM, but the token store is an interface with a
file-backed implementation, so the whole pipeline runs with **no AWS account and no AWS
credentials**:

```sh
export SPOTISTATS_DDB_ENDPOINT=http://localhost:8000        # DynamoDB Local
export SPOTISTATS_TOKEN_FILE=./.dev/refresh_token.json      # 0600, unencrypted
export SPOTISTATS_TABLE_NAME=spotistats
export SPOTISTATS_CLIENT_ID=...                             # else read from SSM
export SPOTISTATS_CLIENT_SECRET=...

spotistats auth login    # one-time, opens a browser
spotistats poll          # ingests real plays into DynamoDB Local
```

`SPOTISTATS_DDB_ENDPOINT` also bypasses the AWS credential chain in favour of static
throwaway credentials, so an expired SSO session is irrelevant. The file-backed token store
is development-only: it is unencrypted at rest, which is why it is opt-in via an explicit
path rather than a fallback.

`SPOTISTATS_SPOTIFY_BASE_URL` and `SPOTISTATS_TOKEN_URL` point the client at a stand-in for
the Spotify API. They exist so the CLI can be exercised end to end in CI, where the real API
is unreachable and needs a human to authorise. Leave both unset in production.

---

## 9. Infrastructure (CDK, Go)

`github.com/aws/aws-cdk-go/awscdk/v2`. Region `us-east-1` (§3.1). One stack, since
there is one environment and the resources share a lifecycle.

`SpotistatsStack`:

| Resource | Configuration |
|---|---|
| DynamoDB `spotistats` | On-demand, PITR on, `GSI1`, `RemovalPolicy.RETAIN`, contributor insights off |
| `capture-lambda` | Go `provided.al2023`, **arm64**, 512 MB, 120s timeout, reserved concurrency 1 |
| `rollup-lambda` | Go `provided.al2023`, arm64, 1024 MB, 900s timeout, reserved concurrency 1 |
| `query-lambda` | Go `provided.al2023`, arm64, 512 MB, 10s timeout, SnapStart n/a for Go |
| EventBridge rules | `rate(2 hours)` → capture; `cron(15 3 * * ? *)` → rollup |
| SSM Parameters | `/spotistats/spotify/client_id`, `/client_secret`, `/refresh_token` — all `SecureString` |
| S3 `spotistats-web` | Private, versioned, encrypted, no public access, OAC-only |
| API Gateway HTTP API | Single `$default` stage, throttling (§10.3), access logs on |
| CloudFront | Two origins (S3 via OAC, API GW), custom domain, TLSv1.2_2021, HTTP/3, IPv6, compression |
| ACM certificate | `us-east-1`, DNS-validated |
| Route 53 | A + AAAA alias records → CloudFront |
| CloudWatch | Log groups (14-day retention), alarms and dashboard (§10.2) |
| AWS Budgets | $10/month with an 80% email alert |

### 9.1 CloudFront behaviours

| Path pattern | Origin | Cache policy |
|---|---|---|
| `/api/*` | API Gateway | TTL 0/60/3600, forward query strings, no cookies |
| `/data/*` | S3 | TTL 0/300/86400 |
| `/assets/*` | S3 | TTL 1y, immutable |
| `/*` | S3 | TTL 0/60/300, SPA fallback |

**SPA routing:** 403 and 404 from the S3 origin rewrite to `/index.html` with status
200. This must be scoped to the S3 behaviours only — applying it to `/api/*` would
turn API 404s into HTML, which is a genuinely confusing failure mode.

Security headers via a CloudFront response-headers policy: HSTS (2y, includeSubdomains,
preload), `X-Content-Type-Options: nosniff`, `Referrer-Policy: strict-origin-when-cross-origin`,
`X-Frame-Options: DENY`, and a CSP of
`default-src 'self'; img-src 'self' https://i.scdn.co data:; style-src 'self' 'unsafe-inline'`.
`i.scdn.co` must be allowlisted or every album cover breaks.

### 9.2 SSM parameter choice

SSM Parameter Store `SecureString` is used rather than Secrets Manager: functionally
equivalent here, but $0/month against ~$0.40/secret/month. Automatic rotation — Secrets
Manager's main advantage — is not applicable, since Spotify refresh-token rotation is
driven by Spotify's own responses and handled in code (§2.6).

---

## 10. Security & operations

### 10.1 IAM

Least privilege per function, no shared role:

| Principal | Permissions |
|---|---|
| `capture-lambda` | `dynamodb:{GetItem,PutItem,UpdateItem,Query}` on the table + `GSI1`; `ssm:GetParameter`/`PutParameter` on `/spotistats/spotify/*`; `kms:Decrypt`/`Encrypt` on the SSM key |
| `rollup-lambda` | Above, plus `dynamodb:BatchWriteItem`, `s3:PutObject` on `spotistats-web/data/*`, `cloudfront:CreateInvalidation` |
| `query-lambda` | `dynamodb:{GetItem,Query,BatchGetItem}` **only** — no write permission whatsoever |
| CloudFront OAC | `s3:GetObject` scoped to the one bucket |

`query-lambda` being read-only matters: it is the only component reachable from the
internet, so it must be incapable of mutating anything.

### 10.2 Observability

Alarms (all → SNS → email):

| Alarm | Condition |
|---|---|
| `CaptureFailed` | capture-lambda errors ≥ 1 in 4h |
| `CaptureStale` | no successful capture in 6h (custom metric on `POLL_CURSOR` age) |
| `RollupFailed` | rollup-lambda errors ≥ 1 in 24h |
| `PlaysGapDetected` | ≥ 1 — capture window saturated, plays may have been lost |
| `AggregateDrift` | > 0 — reconciliation had to correct something |
| `TokenRefreshFailed` | ≥ 1 — likely a revoked refresh token, needs manual re-auth |
| `Api5xx` | > 5 in 5m |
| `ApiHighVolume` | > 10k requests in 5m — abuse / runaway cost signal |
| `Api4xxSpike` | > 1k in 5m |
| `BudgetExceeded` | AWS Budgets at 80% of $10 |

Structured JSON logs (`log/slog`) with a correlation ID per invocation. `PlaysGapDetected`
and `TokenRefreshFailed` are the two that need human action; the rest are informational
or self-healing.

### 10.3 Public API exposure without a WAF

WAF is out of scope by choice. The site is public and the API is unauthenticated, so
the residual risk is cost amplification, not data disclosure — everything served is
already public. Compensating controls:

1. **Dashboard needs zero API calls** — the highest-traffic path is pure CDN.
2. **Edge caching** with `s-maxage=3600` means repeat load collapses to cache hits.
3. **API Gateway throttling:** 20 rps steady, 40 burst at the stage level.
4. **Lambda reserved concurrency** caps the blast radius regardless of edge behaviour.
5. **`limit` hard-capped at 500**, cursor-based pagination only — no unbounded scans.
6. **Budget alarm at $10/month** as the backstop.

If abuse does materialise, the cheapest escalation is CloudFront rate-based rules or a
Lambda@Edge token check — a design change, not an emergency.

### 10.4 Backup & recovery

- DynamoDB PITR (35 days) plus a weekly on-demand backup retained 90 days.
- The GDPR export zip is the true cold backup — keep it off-machine.
- `spotistats export` produces a portable JSONL dump.
- Full recovery path: recreate the stack, `backfill import` the export,
  `backfill enrich`, `reconcile --all`, `render`. Everything after the export's cutoff
  is unrecoverable if PITR has lapsed, which is the honest reason to keep the export.

---

## 11. Cost estimate

Steady state, one user, ~100 plays/day, modest public traffic:

| Service | Monthly |
|---|---|
| DynamoDB on-demand (~80k WRU, ~200k RRU) | ~$0.15 |
| Lambda (12 captures + 1 rollup daily + API) | ~$0.00 (free tier) |
| API Gateway HTTP API (~50k req) | ~$0.05 |
| S3 (< 1 GB, few thousand requests) | ~$0.03 |
| CloudFront (< 100 GB) | ~$0.00 (1 TB free tier) |
| Route 53 hosted zone | $0.50 |
| SSM Parameter Store (standard) | $0.00 |
| CloudWatch logs/alarms | ~$0.30 |
| **Total** | **~$1.05/month** |

One-off backfill: ~$3–5 in DynamoDB write units for ~100k plays, plus a few hours of
wall-clock for metadata enrichment under development-mode rate limits.

---

## 12. Repository layout

```
spotistats/
├── docs/
│   ├── SPECS.md
│   └── PREREQUISITES.md
├── cmd/
│   ├── spotistats/          # local CLI
│   ├── capture/             # capture-lambda
│   ├── rollup/              # rollup-lambda
│   └── query/               # query-lambda
├── internal/
│   ├── spotify/             # API client: auth, retry/backoff, batching
│   ├── store/               # DynamoDB repo, key builders, aggregate math
│   ├── ingest/              # capture + export-import pipelines
│   ├── rollup/              # reconcile, leaderboards, snapshot render
│   ├── model/               # shared domain types
│   └── config/              # env + SSM configuration
│   ├── spotify/spotifytest/ # deterministic fakes: clock, scripted HTTP, token store
│   └── store/storetest/     # DynamoDB Local harness, table builder, seed corpus
├── infra/                   # CDK app (Go)
│   ├── main.go
│   └── stack.go
├── web/                     # React + TS frontend
│   ├── src/{routes,components,charts,lib,hooks}
│   └── vite.config.ts
├── Makefile
└── .github/workflows/deploy.yml
```

Shared Go module at the root so Lambdas, CLI, and CDK reuse `internal/`. Frontend is a
separate npm workspace under `web/`.

---

## 13. Delivery milestones

| # | Milestone | Exit criteria |
|---|---|---|
| 1 | Prerequisites | Spotify app created, export **requested**, AWS bootstrapped, domain chosen |
| 2 | Core Go packages | **Done**: `model`, `spotify`, `store` with unit + DynamoDB Local tests |
| 3 | CLI + auth | **Done** (pending the one-time browser step): `auth login`, `auth status`, `poll` built; `poll` verified end to end against a fake Spotify + DynamoDB Local |
| 4 | Infra skeleton | Table, Lambdas, schedules deployed; capture running unattended |
| 5 | Backfill | Export imported, enriched, reconciled; aggregates verified against a hand-computed sample |
| 6 | Query API | All §6.1 endpoints live behind CloudFront with edge caching |
| 7 | Dashboard | Static snapshot rendering + dashboard page against the validated palette |
| 8 | Explorer | Table, filters, query builder, CSV export, deep links |
| 9 | Hardening | Alarms, budget, PITR, security headers, Playwright smoke suite |
| 10 | CI/CD | GitHub Actions via OIDC; push to `main` deploys |

Milestone 1 gates everything and contains a step with up to 30 days of latency — start
it today. Milestones 2–4 do not depend on the export arriving; milestone 5 does.

---

## 14. Open decisions

| # | Decision | Recommendation |
|---|---|---|
| 0 | Go module path | **Decided:** `github.com/neovasili/spotistats`. |
| 0b | `PLAY#` partition timezone | **Decided:** UTC, while every aggregate *period key* is local. The partition is storage addressing, not a semantic period, and decision 4 makes the timezone a runtime setting — local partitions would strand every existing row if it ever changed. A `STATE / CONFIG` row records the configured zone and schema version, and `store.VerifyConfig` turns a mismatch into a startup failure. Cost is one extra partition read, since a local month spans two UTC months. |
| 1 | Which domain / subdomain | Needed before milestone 6. `stats.<domain>` or `spotify.<domain>`. If the domain's DNS is not already in Route 53, §5 of the prerequisites covers both paths. |
| 2 | Capture cadence | Start at 2h. Tighten to 1h if `PlaysGapDetected` ever fires. |
| 3 | Include skips as plays? | Match the API: count ≥30s only. Keep skipped rows in the export import so the definition can be revisited without re-importing. |
| 4 | Timezone for rhythm charts | `Europe/Madrid`, as an env var so it is changeable without a redeploy. |
| 5 | Public or private repo | Public is fine — no secrets in code. Confirm before enabling CI. |
| 6 | Podcast handling | Excluded (API cannot see them). State it in the UI footer. |
