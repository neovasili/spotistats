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
| **Explorer** (`/explore`) | Full catalogue of everything listened to, with play counts, minutes, filtering, and ad-hoc grouped queries. Every query is a shareable URL. | Query API (API Gateway → Lambda → DynamoDB) |

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
- **Fast frontend feedback loop:** the frontend must run locally against a real backend,
  with hot reload, and without a deploy between edit and result. See §7.4.

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

**Design response:** the capture job runs **every 30 minutes**. The *nightly* job is retained
but repurposed: it does rollups, dimension refresh, snapshot rendering, and reconciliation.
Capture frequency and rollup frequency are decoupled. Cost impact is negligible (48 Lambda
invocations of ~2s per day, most of them finding nothing new).

> **Corrected from 2 hours.** The original figure came with the reasoning "12×/day → ~600
> plays/day of headroom", and that is the **wrong quantity**. The constraint is plays per
> *polling window*, not per day: 60 tracks in one evening loses 10 of them however quiet the
> rest of the day was. Production confirmed it — a gap was recorded at a 2-hour interval, at an
> observed rate of ~17 plays/hour (about 34 per window, two-thirds of the page limit).
>
> At 30 minutes, saturating the 50-item page needs a sustained track every 36 seconds, which is
> unreachable. `infra.TestCaptureIntervalGuardsAgainstPageSaturation` pins this so the interval
> cannot be widened back without the reasoning being confronted.

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

**§4.5's external sources do not bring any of them back.** MusicBrainz supplies membership,
origin and formation facts, and TheAudioDB supplies prose and artwork — none of that is
audio analysis, and neither database holds similarity data, so "related artists" stays dead
rather than merely relocated. Enrichment widens what is *known about* an artist; it does not
restore anything in the table above.

#### The February 2026 change — every batch multi-get is gone

A second, larger restriction landed after this spec was first written and invalidated the
enrichment design below. Spotify's [February 2026 Web API change][feb2026] **removed the
batch multi-get endpoints** for Development Mode apps (existing apps migrated 2026-03-09):

| Removed | Surviving replacement |
|---|---|
| `GET /v1/artists` (Get Several Artists) | `GET /v1/artists/{id}` |
| `GET /v1/tracks` (Get Several Tracks) | `GET /v1/tracks/{id}` |
| `GET /v1/albums` (Get Several Albums) | `GET /v1/albums/{id}` |
| `GET /v1/artists/{id}/top-tracks` | — |
| `GET /v1/browse/new-releases`, `GET /v1/markets` | — |

Calling a removed endpoint returns **403 Forbidden** with no informative body. In production
this presented as artist enrichment failing wholesale, and because the artist name was
sourced only from that call, **31 artists rendered as raw Spotify IDs**. See §4.1 step 5.

Consequences now baked into the design:

- **One request per entity, not one per 50.** `internal/spotify` fans out over the
  single-item endpoints. Only new or unenriched entities are ever fetched, so the steady
  state is a handful per capture run, but a *backfill* is linear in artist count and must be
  capped and resumable (`spotistats enrich`).
- **Partial progress must survive failure.** The batch call was all-or-nothing; the fan-out
  returns everything resolved before the error, so each run makes forward progress.
- **404 replaces the positional null** as "this ID does not exist". Any other status is an
  error and must never be tombstoned.
- **`followers` is always null and `popularity` is deprecated.** Neither may back a UI
  element. `genres` is also marked deprecated but is still returned — the genre features
  depend on a field Spotify has signalled it may remove, which is a standing risk, not a bug.
- Development Mode now also requires a Premium account, allows one Client ID, and permits at
  most **five allowlisted users**. A non-allowlisted user gets 403 on user-scoped calls.

[feb2026]: https://developer.spotify.com/documentation/web-api/references/changes/february-2026

Still available and used: `recently-played`, `me/top/{artists,tracks}`, and the single-item
`tracks/{id}`, `artists/{id}`, `albums/{id}` for metadata enrichment.

> **Never send `market` on metadata enrichment.** With a market set, Spotify applies
> *track relinking* and returns a **different track ID than the one requested**, so the
> same song silently accumulates statistics under two identities. No metadata request
> carries it, and a test asserts that. **Genres come from
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

### 2.7 Artwork: free with the payloads we already fetch, for three of five dimensions

Yes — images come back from the Web API, and Spotistats already captures them. This
section records what is actually on offer so the UI work in §7.6 is not designed against
guesses.

**Both enrichment calls already carry images.** The artist and album objects returned by
`GET /v1/artists` and `GET /v1/albums` each include an `images` array of
`{url, height, width}`, documented as *"in various sizes, widest first"*. §4.1 already
issues both calls — artists for genres, albums for release dates — so **artwork costs zero
additional requests** against the §2.5 rate limit. There is no separate artwork endpoint to
call and no reason to want one.

| Dimension | Artwork | Source |
|---|---|---|
| Artist | Yes | `artist.images`, straight off the enrichment call |
| Album | Yes | `album.images`, straight off the enrichment call |
| Track | Yes, **inherited** | Tracks have no `images` field of their own; the cover is the album's |
| Genre | **None, ever** | A genre is a string on the artist row, not a Spotify entity |
| Total | N/A | Not an entity |

> **A track object has no artwork.** The cover shown for a track everywhere in the Spotify
> clients is `track.album.images`. `internal/rollup` resolves it with **one batched album
> lookup per leaderboard**, not one per track — 50 tracks is one `BatchGetItem`, not 50.
> Any future surface that shows track art must reuse that batch, not add a lookup per row.

**Sizes are not part of the contract.** In practice albums return 640/300/64 and artists
640/320/160, but the API documents only the ordering. Never hardcode a dimension and never
rewrite the URL to ask for a different size — Spotify documents no resizing parameter, and a
hand-edited `i.scdn.co` path is an unsupported URL that can 404 without warning. **Select by
`width` from the array**, which is why the array must be the thing consulted at capture time
(§7.6) — after storage, the choice is already made.

**URL lifetime differs by entity, and the difference is documented.** Album and artist image
URLs carry no expiry note and are content-hashed on `i.scdn.co`; they are stable in practice.
**Playlist** cover URLs are explicitly *"temporary and will expire in less than a day"*.
Spotistats stores no playlist artwork today, so this does not currently bite — but it is the
reason a stored `imageUrl` is treated as **a refreshable cache, not an identifier**: no
downstream row keys off it, and the capture job's staleness check (`DimensionStaleAfter`,
30 days) rewrites it from a fresh API response whenever it re-enriches the row.

**Displaying artwork carries obligations the data does not.** The Spotify Developer Policy
requires that cover art and metadata be *"accompanied by a link back to the applicable album,
content or playlist on the Spotify Service"* and attributed to Spotify. That is a UI
requirement, satisfied in §7.6, not something the ingestion pipeline can discharge. The same
policy prohibits ingesting Spotify Content into a machine-learning model — noted here so it
is not rediscovered later as a "nice idea".

**Current state: complete.** Capture → storage → snapshot → API → UI all carry artwork.
`widestImageURL` and `thumbImageURL` map the array to `imageUrl`/`thumbUrl` on the
`ARTIST#`/`ALBUM#` `META` rows (§5.2); both travel through `store.Label`,
`store.LeaderboardEntry`, the snapshot `Entry` and the `/list`, `/top` and `/stats` responses;
and §7.6 renders them.

`thumbImageURL` picks the **narrowest asset at least 160px wide**, reading `width` from the
array rather than assuming a position, and falls back to the widest when nothing qualifies —
including when Spotify omits the dimensions entirely.

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
| **CloudFront fronts both origins** | `/api/*` → API Gateway, everything else → S3. Same-origin means **no CORS anywhere in the system** — not in production, and not for local frontend development either (§7.4). API responses also cache at the edge. |
| **Aggregates maintained at write time** | The target query ("minutes of Within Temptation in 2025") becomes a single `GetItem` instead of a scan. See §5.3. |
| **Nightly reconciliation** | Write-time counters can drift if a Lambda dies mid-update. Raw play events are the source of truth; aggregates are a cache that gets rebuilt. See §4.3. |
| **Backfill runs locally, not in Lambda** | The import is a one-off, needs a multi-GB local file, and takes far longer than the 15-minute Lambda ceiling. Local execution also avoids exposing a privileged bulk-write endpoint to the internet. |
| **Two stacks, two regions** | Data and compute live in **`eu-west-1`**, close to the listener. CloudFront accepts an ACM certificate only from **`us-east-1`**, so the certificate — and the billing budget, which is likewise global — live in a separate `SpotistatsGlobalStack` there. Exactly one value crosses regions: the certificate ARN, passed via CDK's `crossRegionReferences`. |
| **CloudFront stays with its S3 origin** | Despite CloudFront being a global service, its construct lives in the regional stack. It has to: the Origin Access Control grant is a bucket policy naming the distribution's ARN, while the distribution names the bucket's domain. Split across stacks those references form a cycle CDK cannot resolve. Keeping them together is what holds the cross-region surface down to a single value. |

---

## 4. Data ingestion

### 4.1 Capture pipeline (`capture-lambda`, every 30 minutes)

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
5. **Resolve artists — before recording anything.** This has two independent halves, and
   separating them is load-bearing.
   - **Names come free, from the page.** Every recently-played track embeds *simplified*
     artist objects: ID and name, no genres. Persist those first, via a write that sets
     only the name (`PutArtistName`). An earlier draft of this spec discarded them on the
     grounds that they "carry no genres" — true, but it made the artist's **display name**
     depend on the enrichment call below succeeding. When that call failed in production
     the dashboard rendered 31 raw Spotify IDs, because no artist row existed at all. Names
     must not depend on a second API call.
   - **Genres need `GET /v1/artists`.** Batch-read the artist rows for every artist on the
     page; fetch and persist any not yet enriched or gone stale.
   - **`enrichedAt` is separate from `refreshedAt`.** A row can be freshly written and never
     enriched, because the two halves above have different sources. The enrichment gate must
     test `enrichedAt`; gating on the row's age would let a name-only stub suppress the genre
     fetch permanently.
   - **The name write must be an `UpdateItem`, not a `PutItem`.** A Put would overwrite an
     already-enriched row's genres, popularity and images with the empty fields of a
     simplified object, so every poll would undo the previous enrichment. It must also leave
     `enrichedAt` untouched, per the point above. Note that `name`, `type` and `missing` are
     all DynamoDB **reserved keywords** and must be aliased in any expression.
   - Spotify returns positionally-aligned `null` for IDs it cannot resolve (removed,
     relinked, invalid). Those get a **tombstone** `META` row with `missing: true`.
     Without it the enrichment pass would re-request the same dead IDs on every run
     forever, burning quota that development mode cannot spare.
   - **Tombstones expire** (`TombstoneRetryAfter`, 7 days — deliberately shorter than the
     30-day name refresh). A tombstone is a *negative* cache entry and unlike a cached name
     it can be wrong: it is written for a transient upstream fault or a short response array
     exactly as for a genuinely dead ID. A permanent tombstone turns any such blip into an
     entity that is nameless and genre-less forever. Re-asking about a few IDs is far cheaper
     than that.
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

### 4.2 Backfill pipeline (local CLI, one-off) — **implemented**

```
spotistats backfill --path ./.dev/historic-data [--dry-run] [--min-ms N] [--rps N]
                    [--enrich-only] [--enrich-limit N] [--skip-enrich]
spotistats backfill-prune --from <ts> --to <ts>
```

Measured against the real export: **439,303 records, 2009-11-01 → 2026-08-21, 13,169 unique
tracks** after filtering.

**1. Scan.** Stream-parse every `Streaming_History_*.json`. Report the corpus — records,
importable count, unique tracks, coverage window, and every skip broken out by reason — and
write nothing. `--dry-run` stops here and needs **no table, no credentials and no network**,
because inspecting the corpus is exactly what you do before deciding to commit.

> Both `Audio` and `Video` files are read. The Video ones are **not** podcasts: they are music
> tracks played with a video stream (`reason_start="switched-to-video"`) carrying an ordinary
> `spotify_track_uri`, and skipping them would silently drop real listening.

**2. Filter.** Drop podcasts and audiobooks (by `spotify_episode_uri` / `audiobook_uri`, checked
**before** the track URI — an audiobook chapter can carry one), rows with no track URI, rows
with an unparseable `ts`, and anything under `--min-ms` (default 30,000).

> ⚠️ **The original justification for the 30s threshold was wrong.** This spec claimed it
> "matches the API's ~30s threshold". `recently-played` has no such threshold: it records a
> track when it is played **to completion**, so joining a song for its final five seconds
> registers a play while abandoning one after four minutes does not.
>
> The faithful equivalent is therefore `reason_end == "trackdone"`, and it was measured against
> this corpus: it would discard **1,440 hours — sixty days — of genuinely attended listening**,
> because a track played for four minutes and skipped ends `fwdbtn` and vanishes. Seventeen
> years of real history is not worth sacrificing for comparability with the single day the API
> era covers.
>
> The threshold stays, for the honest reason: it removes sub-30-second noise while keeping
> **99.8% of attended time**. The resulting asymmetry is real and disclosed — in the API era a
> play is a completion, in the export era a listening stretch of at least `--min-ms`.

**3. Identity — names now, Spotify IDs as they arrive.**

The export identifies the track (`spotify_track_uri`) but names artists and albums only as free
text. Every aggregate is keyed by Spotify ID (§5.2), so resolution matters: the first import ran
without it and produced **correct totals with 15% artist attribution**, which made the artist
leaderboard not merely short but *wrong* — the true top artist was absent from the top five and
those shown read at a quarter of their real totals, while looking entirely plausible.

> ⚠️ **Resolving every track through the API is not feasible.** It is one request per unique
> track — 13,169 of them — and Spotify's development mode answered ~500 requests with a **429
> carrying `Retry-After: 7h30m`**. Reported dev-mode cooldowns run 13–18 hours and the quota is
> unpublished, so a full pass is weeks of dripping. This spec previously assumed the resolution
> was merely "the slow part".

So identity is **late-bound**, and resolved in this order:

1. the track's dimension row, when enrichment has written real Spotify IDs;
2. otherwise a **name key** derived from what the export supplies.

`model.NameKey` folds case, Latin diacritics and whitespace and prefixes `nm:` — which is not a
valid Spotify ID (those are 22 base62 characters), so a name-keyed row can never be mistaken for
a real one. Album keys fold in the artist, because "Greatest Hits" alone would merge dozens of
unrelated records.

Folding is a **repair**, not a compromise: measured over the export's 3,751 distinct artist
names it merges exactly three pairs, and all three are one artist spelled two ways — `Ayo`/`Ayọ`,
`JAY-Z`/`JAŸ-Z`, `Nino Bravo`/`Niño Bravo`. The export writes `Heroes Del Silencio` where the API
returns `Héroes del Silencio`, and without folding those two would split one artist's history
exactly as a forked ID space would. The accepted residual risk is that two genuinely different
artists sharing a name merge — rare in a personal library, and far less wrong than attributing
85% of plays to nobody.

**Late binding is what makes this safe.** Attribution is *not* written into the play row.
`model.FactsForTrack` resolves it at aggregate time, so when enrichment eventually resolves a
track, the next reconcile switches seventeen years of aggregates onto real Spotify IDs — with no
reimport of 400,000 plays. The play row carries the export's `trackName`, `artistName` and
`albumName` precisely so that resolution stays possible later.

The importer also writes **placeholder dimension rows** for name-keyed artists and albums, and
for unresolved tracks, using the export's original casing. Without them the dashboard would
render `nm:within temptation` instead of `Within Temptation`. A placeholder track keeps
name-keyed attribution so `Enricher.unresolved` still recognises it as needing the API —
otherwise a placeholder would look resolved and the track would stay on fallback identity
forever, which is the state enrichment exists to escape.

**Enrichment therefore becomes an optional upgrade rather than a prerequisite**, and remains
worth running: it replaces name keys with real Spotify IDs, and brings artist images with them.

One `GET /v1/tracks/{id}` answers all of it: the full track object embeds its album (ID, name,
images) and its artists (IDs, names), so a single request per unique track populates all three
dimensions. `spotify.TrackDetail` exists for exactly this — `model.Track` keeps only `AlbumID`
and `ArtistIDs`, so mapping through it would discard the names that already arrived.

`ArtistCoverage` on the coverage row is the exact share of listening time carrying **rankable**
attribution — a play counts only when at least one of its artists is a real Spotify ID. The
dashboard shows a caveat on the artist and album cards below 99% and drops it automatically at
full coverage, so the disclaimer disappears on its own rather than becoming furniture.

> **The word "rankable" is load-bearing, and this figure originally got it wrong.** It counted
> any attribution, including a name key — so it read **1.00** while 39% of listening time sat on
> name-keyed rows and the artist rankings were split across two entries per artist. Every other
> surface agreed the data was fine: `doctor`'s name-resolution check reported every leaderboard
> entry as *named*, because a name-keyed artist has a perfectly good display name — the name IS
> its identity — and the canonicaliser silently merged whichever names some other resolved track
> happened to supply a mapping for. Nothing anywhere reported the split.
>
> What the caveat needs to know is not "does this play name an artist" but "can it be compared
> with the others", and only a resolved ID answers that: two name keys that are really one artist
> divide its history, and no downstream check can see it. `spotistats doctor` now reports the
> name-keyed share per dimension directly, with the biggest offenders named.

#### 4.2.1 Track identity is a long tail, not a step

The import writes a **placeholder** track row for every track: a display name from the export,
and `artistIds` / `albumId` that are name keys rather than Spotify IDs. Resolution then upgrades
them one request at a time, and that pass is the slowest thing in the system.

Measured on the real corpus: **12,140 of 13,169 tracks** are still placeholders, and Spotify's
development-mode quota allows roughly **500 requests per rate-limit window**, with the 429
carrying a `Retry-After` of **7.5 to 18 hours**. So the backlog is ~25 windows — weeks of
wall-clock, not an afternoon. A naive reading of a short probe suggests otherwise: 250 requests
at 3 req/s complete in 89 seconds and look like a 70-minute job for the whole corpus, right up
until the quota is spent.

Consequences that shape the design:

- **Budgeted, and it stops ASKING rather than merely stops succeeding.** Resolution shares the
  Spotify quota with capture, and capture is the one job that must not fail: `recently-played` is
  a rolling ~50-play window, so consecutive capture failures lose listening **permanently** and no
  reconcile can recover it. So a 429 writes a `STATE / RESOLVE_COOLDOWN` row carrying the API's
  own `Retry-After`, and the next run exits without issuing a single request — every request made
  during a cooldown is quota taken from capture for nothing.
- **It runs nightly at 05:15 UTC, after the rollup and the external enrichment**, at a default of
  200 tracks (`backfill.DefaultResolveLimit`). The figure is set by what it must not do rather
  than by how fast it could go: capture spends ~150 requests a day of an observed ~500-650, so 200
  leaves headroom that keeps the resolver from ever provoking the 429 in the first place. The
  honest cost is ~2 months to drain the backlog unattended, against ~25 days of *daily* manual
  runs — unattended and slow beats fast and requiring discipline.
- **`spotistats resolve` is the same code path**, for an operator who wants to spend quota now:
  `--limit -1` for everything remaining, `--force` to override a cooldown that recovered early,
  `--dry-run` to size the backlog without spending anything. The CLI and the Lambda share
  `backfill.Resolver` deliberately — the quota-safety rules are the whole substance of this job,
  and a second copy of them would eventually disagree with the first.
- **`ResolveRemaining` is the metric that answers "will this ever finish?"** without anyone
  running a command. It should fall monotonically; a flat line across days means runs are being
  suspended by a cooldown they never escape, which `ResolveStalled` alarms on after three days.
- **The work list comes from the STORE, not the export.** `backfill.PlayedTrackIDs` reads the
  all-time track aggregate, which is the same set by construction. Deriving it from the export
  tied resolution to a 330MB directory on one laptop — fine for a one-off import, useless for a
  tail that outlives it.
- **Most-played first.** Under a hard quota the ordering *is* the strategy: resolving the artist
  behind a thousand plays fixes a leaderboard row; resolving a one-play curiosity fixes a
  rounding error.
- **Resolution alone changes nothing visible.** It rewrites track rows; a full
  `spotistats rollup --all` is what moves seventeen years of artist and album aggregates onto the
  new identity. That is the same property that makes the whole scheme work — see below.

- **Resumable with no cursor row.** The dimension rows *are* the cursor: a pass reads which
  track IDs already exist and skips them, so an interrupted run resumes by being run again and
  there is no state to go stale. Tombstoned (catalogue-removed) tracks are never re-requested.
- **Throttled.** One request per track means thousands back to back, so the client is given a
  window limiter (`--rps`, default 3). Unthrottled this walks into a 429 storm; the retrier
  would survive it but the run would spend its life in backoff.
- Ordering matters: a play row denormalises its album and artist IDs, so a play written before
  its track is resolved carries no attribution and would need **reimporting**, not merely
  reconciling.

**4. Import.** Write play rows with `source=export`, `msEstimated=false`, exact `msPlayed`, via
`BatchWriteItem` — 25 per call rather than 400,000 sequential `PutItem`s.

**No aggregate deltas are applied during the import.** At roughly 14 deltas per play that would
be ~6 million writes; recomputing once from the play rows afterwards costs a fraction of it, and
the play rows are the source of truth in any case. The import is therefore followed by:

```
spotistats rollup --all --timeout 2h
```

This is required, not optional. It is also why the import needs no aggregate-merge arithmetic:
the periods `ALL`, the current year and the current month span **both** sources, and writing them
from export-only data would destroy the API era's contribution.

**5. Source precedence — and the destructive version of this rule.**

> ⚠️ This spec originally said: *for any month claimed by an `INGEST#{month}` marker with
> `source=export`, the importer **deletes** `source=api` play rows in that month.*
>
> That is **data-destroying here.** The export ends 2026-08-21 and capture began 2026-08-22, so
> the two share the month of August while overlapping on not a single play — and a month rule
> would delete every captured play from the days after the export ends, which exist in no other
> source and cannot be re-fetched.

The window is `[first, last]` of what was **actually imported**. Inside it the export is
authoritative, because its `ms_played` is exact where the API assumes the track's full duration.
Outside it the API is the only source and is left alone.

Deletion is never automatic: `backfill` *reports* how many API rows fall inside the window, and
`backfill-prune` (separate command, bounded window required, confirms first) performs it. Writing
hundreds of thousands of rows and deleting some of them are different risks and do not belong in
one step.

### 4.3 Nightly job (`rollup-lambda`, 03:15 UTC)

1. **Reconcile.** Repair aggregate drift over the trailing 45 days. `AggregateDrift` reports
   how many rows were corrected; non-zero means a capture run died between the play insert and
   the aggregate update — rare, but this makes it self-healing rather than permanently wrong.

   **An earlier draft said "recompute aggregates for the trailing 45 days and compare to
   stored counters". That is not implementable.** `AGG#TRACK#ALL` covers every play ever
   recorded, so no windowed read can recompute it; comparing a window's worth of plays against
   an all-time counter would report enormous phantom drift and then "correct" the counter to
   the window's value, destroying the history it was meant to protect. What actually happens:

   1. Recompute the finest granularity the window fully determines — the month rows for every
      month the window touches, read **in full**, plus the `TOTAL` day rows.
   2. For each, compute `correction = recomputed − stored`.
   3. Apply that correction to the year and all-time rows with an atomic `ADD`.

   Step 3 is what makes it correct: a **delta** is meaningful against a counter of any span,
   whereas an absolute value from a partial read is not. The hierarchy is repaired without
   reading history, and the cost is bounded by the window rather than the dataset. A row with
   no plays behind it any more is zeroed, or a deleted play leaves a phantom entity in every
   leaderboard forever. Drift originating outside the window needs `spotistats rollup --all`,
   which is manual because a full pass rewrites every aggregate row.

   A regression test asserts a narrow window never moves an all-time counter.
2. **Refresh materialised leaderboards** (`TOP#*`) for the affected periods.
3. **Refresh top-items** from `me/top/{artists,tracks}` for all three
   `time_range` values (`short_term` ≈ 4 weeks, `medium_term` ≈ 6 months,
   `long_term` ≈ 1 year). These are Spotify's own rankings and are stored alongside
   Spotistats' computed ones — they will differ, and the UI labels which is which.
4. **Render snapshots** to S3: `data/dashboard.json`, `data/catalog.json`, `data/meta.json`.
   Written with `Cache-Control: public, max-age=300, s-maxage=86400` followed by a targeted
   CloudFront invalidation of `/data/*`. Targeted because invalidating `/*` would evict the
   site's hashed assets, which are immutable and never need it.

### 4.4 The coverage pass, and two figures that cannot be derived

Steps 2 and 3 above need one more thing, and it is worth stating because both figures were
initially got wrong and only surfaced by running the job against real data:

- **The all-time `firstPlayedAt`/`lastPlayedAt` are not the coverage window.** They are
  best-effort write-time attributes (§5.2): `firstPlayedAt` uses `if_not_exists` and so records
  the first play *written*, not the earliest played. Out-of-order ingestion — a backfill, a
  replay, a reconcile — leaves them badly astray, and a windowed reconcile cannot fix an
  all-time bound.
- **Genre coverage cannot be summed from the genre aggregates.** A play whose artists carry
  three genres contributes to three rows, so the sum overstates coverage; capping it at the
  total then reports a confident 100% whenever the overcount exceeds the shortfall.

Both need a per-play pass over the whole history, so the histogram refresh — which already
streams every play — computes them in the same pass and writes a `STATE / COVERAGE` row. The
dashboard prefers that row and marks the window `approximate` when it is absent, rather than
presenting write-order artefacts as fact.

---

### 4.5 External enrichment: MusicBrainz + TheAudioDB — built

Spotify's artist object carries a name, genres, popularity, followers and one photo, and
**nothing else** (§2.7). No biography, no members, no formation date, no origin. Those facts
exist in two free databases, and — the part that makes this worth building — both join to a
Spotify artist **exactly, without ever matching on name**.

#### 4.5.1 Division of labour

Neither source is a superset of the other, and the split is not a matter of taste:

| Field | Source | Why not the other one |
|---|---|---|
| Members — names, instruments, tenure | **MusicBrainz** | TheAudioDB returns `intMembers`, a *count* (`4`), with no names |
| Started / formed | **MusicBrainz** `life-span.begin` | Date-precision aware; TheAudioDB's `intFormedYear` is a bare year and is often wrong |
| Origin / nationality | **MusicBrainz** `country`, `area`, `begin-area` | ISO 3166-1 code plus a *city* of origin; TheAudioDB's `strCountry` is free text ("London, England") |
| Genres | **MusicBrainz** `genres` | Vote-counted list; TheAudioDB gives one `strGenre` plus one `strStyle` string |
| Biography / description | **TheAudioDB** `strBiography` | MusicBrainz holds no prose — annotations are rare and unstructured |
| Photos, fanart, logos, banners | **TheAudioDB** | MusicBrainz has **no artist images at all** (Cover Art Archive is releases only) |

> **MusicBrainz wins every structured fact; TheAudioDB wins prose and imagery.** This is not
> a preference, it is what the data forces. TheAudioDB's Coldplay record returns
> `intFormedYear: 1996` while its own biography says "formed in London in 1997", reports
> `strGender: "Male"` for a four-piece band, and `strTwitter: "1"`. It is a fan-curated
> artwork database with metadata attached, and it is excellent at the artwork. Treat its
> structured fields as a **fallback only where MusicBrainz is empty**, never as an override,
> and record which source each block came from.

#### 4.5.2 The join, and why it never guesses

Spotify IDs mean nothing to either database. The chain is two exact lookups:

**Step 1 — Spotify artist ID → MBID**, via the MusicBrainz URL entity:

```
GET /ws/2/url?resource=https%3A%2F%2Fopen.spotify.com%2Fartist%2F{spotifyId}
             &inc=artist-rels&fmt=json
→ urls[].relations[] where type == "free streaming" → relations[].artist.id  (the MBID)
```

MusicBrainz editors record the Spotify page as a URL relationship on the artist, so this is a
**link someone asserted**, not a string similarity. **The `resource` parameter repeats up to
100 times in one request**, and the response comes back as a `urls[]` array keyed by
resource — so 2,000 artists resolve in 20 requests, roughly 20 seconds at the rate limit,
not 2,000 seconds. Batch it; a per-artist loop here is the difference between a 30-second job
and a 30-minute one.

> **The response shape changes with the batch size.** With two or more `resource` parameters
> the body is `{"url-count", "url-offset", "urls": [{resource, relations}, …]}`. With
> **exactly one**, MusicBrainz returns the bare URL entity instead — `relations` at the top
> level, no `urls` wrapper. A client written against the batch shape decodes an empty result
> for a batch of one, and a batch of one is not exotic: it is the tail chunk of any artist
> count that is not a multiple of 100, and it is what a `--artist` single-artist run always
> sends. Handle both shapes, and cover the one-element case in a test.

**Step 2 — MBID → TheAudioDB**, which indexes by MBID directly:

```
GET /api/v1/json/{key}/artist-mb.php?i={mbid}
```

> **There is no name-search fallback, deliberately.** `search.php?s={name}` exists and it is
> tempting for the artists MusicBrainz has not linked. Do not wire it in. A fuzzy name match
> attaches the wrong biography, the wrong members and the wrong country to an artist, and
> **nothing downstream can detect it** — the profile page renders a confident, wrong answer
> about a real band. That is precisely the failure the §6.4 honest-metrics contract exists to
> prevent. An unresolved artist renders no profile, which is a visible gap the reader can
> interpret. Mis-resolution is a lie the reader cannot see.
>
> The escape hatch is manual, not automatic: `spotistats mbid set {spotifyId} {mbid}` writes
> an override the resolver consults first, so a specific artist can be fixed by hand by
> someone who actually checked.

#### 4.5.3 Why this is its own pipeline

Capture's contract is *never lose a play* (§4.1). MusicBrainz allows **1 request per second
per IP** and answers `503` to *everything* from that IP once exceeded; TheAudioDB allows **30
requests per minute** on the free key and answers `429`. Putting either in the capture path
would make play durability depend on two third-party services with hard throttles and no SLA.

So external enrichment is a **separate, resumable, budget-bounded job** that can fail
completely without touching a single play, aggregate or leaderboard. It runs daily at
**04:15 UTC** — an hour after the rollup (§4.3), never overlapping it — and its work list is
the `AGG#ARTIST#ALL` partition, exactly as the existing `spotistats enrich` command sources
Spotify enrichment. That partition is every artist ever played, so coverage is complete
rather than limited to whoever currently charts.

#### 4.5.4 Rate limits, etiquette, and the concurrency trap

| | MusicBrainz | TheAudioDB |
|---|---|---|
| Limit | 1 req/s per IP (average) | 30/min free key, 100/min premium, 120/min business |
| Over-limit | `503` on **all** requests from the IP | `429` |
| Auth | None | Key in the path; `123` is the public test key |
| `User-Agent` | **Mandatory and meaningful** | Not enforced |

- MusicBrainz **requires** a descriptive `User-Agent` with contact information —
  `spotistats/1.0 ( https://spotistats.neovasili.com )`. Anonymous and default library agents
  (`Python-urllib`, `Java`, blank) are throttled far harder as a class. The client constructor
  must **fail fast** if no contact string is configured rather than sending a default one.
- Treat MusicBrainz `503` as **backpressure, not failure**: back off and retry, do not
  surface it as an error. The existing retry policy already does exactly this for Spotify's
  `429`, including `Retry-After` in both RFC 7231 forms.
- **Reserved concurrency must be 1 on the enrich Lambda.** Both limits are per-IP, and a
  self-imposed 1 req/s limiter inside the process is meaningless if two invocations overlap —
  two concurrent runs double the real rate and earn a `503` for everything. This is a
  one-line CDK property whose absence is invisible until the job starts failing wholesale.

#### 4.5.5 Storage: a second item, not a fatter `META` row

External data goes on a **new sort key**, `ARTIST#{id} / EXTERNAL`, beside the existing
`META` row (§5.2):

```
PK  ARTIST#{id}   SK  EXTERNAL
    mbid, mbResolvedVia (link|override), mbGenres[], artistType (Group|Person),
    country (ISO), areaName, beginAreaName, beganAt, beganPrecision,
    endedAt, ended, members[{name, mbid, instruments[], begin, end}],
    audiodbId, biography, biographyLang, images{thumb, logo, cutout, clearart,
    wideThumb, banner, fanart[]}, sources{facts, prose, images}, refreshedAt
```

Three reasons it is not merged into `META`:

1. **`META` is on the hot path.** Every leaderboard hydration reads artist rows 50 at a time
   (§4.3). A biography is multi-kilobyte — Coldplay's English text alone is ~3.5 KB — and
   DynamoDB bills reads per 4 KB, so folding prose into `META` would roughly double the read
   cost of every leaderboard for text that only one page ever renders.
2. **Different lifecycles.** `META` refreshes every 30 days (`DimensionStaleAfter`) because
   popularity and follower counts move. Formation year, origin and founding members do not
   move; **180 days** is the right staleness window for `EXTERNAL`, and one row cannot carry
   two refresh policies.
3. **Different failure domains.** A row that resolved on MusicBrainz but 429'd on TheAudioDB
   is a normal, partially-populated `EXTERNAL` row. It must not be able to corrupt or block
   the `META` row that genre attribution depends on.

**Store one language, not fifteen.** TheAudioDB returns `strBiography` plus 14 translations
(`strBiographyDE`, `strBiographyFR`, `strBiographyJP`, …). The dashboard is single-language;
persisting all of them multiplies the item ~15× for content nothing renders. Store the
configured language with a fallback to English, and record which one in `biographyLang`.

**Unresolved artists get a tombstone, not a retry loop.** An artist MusicBrainz has never
linked will never resolve, and re-asking every night is a request spent on a known answer.
Write the `EXTERNAL` row with `mbid` empty and a `refreshedAt`, and let it expire on the same
principle as the existing dimension tombstones — negative caching that can be wrong, so it
expires rather than being permanent.

#### 4.5.6 The genre trap — resolved: MusicBrainz genres now FEED `AGG#GENRE`

**Built.** Genre aggregation reads MusicBrainz genres off `ARTIST#/EXTERNAL`. The prohibition
below stood for a while and is kept, because the *reason* for it still binds — it just no longer
applies to the present situation.

The original rule, and why each half was written:

> **Do not feed MusicBrainz genres into `AGG#GENRE#{period}`.** Two independent reasons:
>
> 1. **Different taxonomies, silently double-counted.** MusicBrainz tags Amaranthe *melodic
>    death metal*, *power metal*, *symphonic metal* and *trance metal*; Spotify's vocabulary
>    is its own and rarely spells the same idea the same way. Merged, one play lands under
>    both vocabularies' strings and the genre board reports two entities where one artist
>    exists — and genre rows already legitimately over-sum the total (§7.3), so nothing
>    downstream would flag it.
> 2. **It rewrites history.** Genre aggregates are written at play-ingest time. Adding a
>    second source makes every play recorded *after* the change carry attribution that plays
>    recorded *before* it do not, so the all-time board becomes a mix of two definitions with
>    no marker saying where the seam is.

Both objections were answered rather than ignored:

- **Reason 1 was about MERGING, and there is nothing to merge with.** Every artist row in
  production carries an EMPTY Spotify genre list — verified across all 229 artists that have a
  Spotify ID. MusicBrainz is the *sole* source, which is one coherent vocabulary rather than a
  blend of two. This is the narrow condition under which the rule lifts, and it re-imposes the
  moment Spotify restores its field: at that point pick one source or label each chip by source
  as §7.7 does, but never sum across both.
- **Reason 2 was answered by paying the cost it named.** A full `spotistats rollup --all`
  recomputed every aggregate from the complete history — 408,845 plays, 191,679 rows rewritten —
  so there is no seam between old and new definitions. The all-time board is entirely
  MusicBrainz.

What it actually delivers, and the honest limit:

| | |
|---|---|
| `genreCoverage` | **0.564** — was 0.0, since Spotify's field is gone |
| Not covered | 3,190 name-keyed artists, 39% of listening time |
| Why they cannot be covered | MusicBrainz resolves through the Spotify URL relationship, so an artist imported by name has no ID to relate — and there is deliberately no name search (§4.5.2) |
| Genre *set* | **Robust.** Splitting the 152 genre-bearing artists into disjoint halves and ranking each independently reproduces 7 and 9 of the top ten |
| Genre *order* | **Not robust.** The two halves disagree about first place |

That last row is why the dashboard caveat names the ordering specifically rather than gesturing
at incompleteness: "which genres appear is reliable; their exact order is not". It is the same
rule the artist ranking follows (§4.4), and it hides itself above 99% coverage.

#### 4.5.7 Licensing and attribution

- **MusicBrainz core data** — artists, relationships, life-spans, areas — is **CC0**, public
  domain, no attribution required. The **remaining** portions, explicitly including tags and
  ratings, are **CC-BY-NC-SA 3.0**: attribution required, non-commercial only. MusicBrainz
  genres derive from genre tags, so treat them as falling on the NC side. Spotistats is a
  personal, non-commercial dashboard, so this is compatible.
  **The attribution follows the data onto whatever page shows it.** While genres were
  display-only this was satisfied by the artist profile's footer (§7.7); now that they feed the
  genre chart (§4.5.6), the dashboard footer carries the credit and the licence link too. It
  renders only when there are genres to attribute, so it never claims a source for data that is
  absent.
- **TheAudioDB** requires that they be credited as the source of the data with a link back to
  their site, most of their artwork being user-created. Trademarked logo images must be used
  **as-is and unmodified**, which rules out recolouring or compositing `strArtistLogo`. Some
  artwork carries a `strCreativeCommons` marker; where it is absent, treat the image as
  "displayable with credit", not as freely reusable.
- Both credits belong in the artist profile footer alongside the existing Spotify
  attribution (§7.6) — three sources, three named credits, each linked.

#### 4.5.8 Implementation steps

**Phase A — transport (prerequisite for both clients)**

1. Extract `internal/httpx/` from `internal/spotify`: `RetryPolicy`, `backoff`,
   `parseRetryAfter`, the `retrier` and `Limiter`/`NewWindowLimiter`. All of it is already
   generic HTTP — nothing in it references Spotify — and it is about to have a third and
   fourth consumer. Keep the existing `spotify` symbols as thin aliases so no call site in
   `internal/ingest` or `cmd/` moves in the same commit as the extraction.
2. Add `httpx.RequireUserAgent(s string)` so a client can refuse to construct without a
   contact string. MusicBrainz needs it; making it a transport concern keeps that rule out of
   the caller.

**Phase B — MusicBrainz client (`internal/musicbrainz/`)**

3. `client.go` — base URL, mandatory `User-Agent`, `httpx` limiter fixed at **1 req/s**, and
   a retry policy that treats `503` as retryable backpressure.
4. `dto.go` — wire structs for the URL lookup and the artist lookup. Omit every field not
   used, matching the `internal/spotify/dto.go` convention.
5. `ResolveSpotifyArtists(ctx, spotifyIDs []string) (map[string]string, error)` — chunks IDs
   into batches of **100**, issues one URL lookup per batch, and returns Spotify ID → MBID for
   the `type == "free streaming"` relations. Missing IDs are simply absent from the map.
6. `Artist(ctx, mbid)` — `GET /ws/2/artist/{mbid}?inc=genres+artist-rels&fmt=json`.
7. `mapper.go` — wire → `model.ArtistProfile`. Two rules that are easy to get wrong:
   - **Filter members by direction, not just type.** On a `Group`, members are the
     `artist-rels` entries with `type == "member of band"` **and `direction == "backward"`**;
     on a `Person`, `direction == "forward"` yields the bands they belonged to instead.
     Filtering on `type` alone stores bands as members of people.
   - **`life-span.begin` has variable precision** — `2008`, `2008-04` or `2008-04-17`,
     exactly like Spotify's release date. Store the string **verbatim** plus a precision
     field; parsing it into a `time.Time` invents a day that the data does not claim. Reuse
     the `ReleaseDate`/`ReleaseDatePrecision` pattern already in `model.Album`.
8. `musicbrainztest/` — scripted HTTP fake plus golden JSON in `testdata/`, captured from
   real responses, mirroring `spotifytest/`.

**Phase C — TheAudioDB client (`internal/theaudiodb/`)**

9. `client.go` — key from config, limiter at **30 req/min**, `429` retryable.
10. `ArtistByMBID(ctx, mbid)` against `artist-mb.php?i=`. **Do not implement `search.php`** —
    an unexported, untested absence is the cheapest way to guarantee §4.5.2's no-fuzzy-match
    rule survives a future contributor.
11. `mapper.go` — select the configured biography language with an English fallback; collect
    the ten image fields into a struct, dropping empty strings so absent art is absent rather
    than an empty URL the frontend must special-case.
12. `theaudiodbtest/` fake + golden testdata.

**Phase D — orchestration (`internal/enrich/`)**

13. `Enricher` with the same shape as `ingest.Capturer`: injected store, both clients, clock
    and logger.
14. `Run(ctx, opts)` — read the work list from `AGG#ARTIST#ALL`; skip artists whose `EXTERNAL`
    row is fresher than `ExternalStaleAfter` (180 days); batch-resolve MBIDs; then per artist
    fetch MusicBrainz, fetch TheAudioDB, merge under the §4.5.1 precedence rule, and write.
15. **Per-artist errors are logged and skipped, never fatal.** One artist 404ing on
    TheAudioDB must not abandon the other 199. Persist whatever *did* resolve with its
    per-source provenance.
16. Budget and resume: stop at `--limit` artists or a wall-clock deadline, whichever comes
    first, and checkpoint into `STATE / EXTERNAL_ENRICH_CURSOR` so the next run continues
    rather than restarting. The `SKEnrichCursor` pattern already exists — add a sibling
    constant rather than sharing one cursor between two different jobs.
17. Emit EMF metrics: `ExternalArtistsResolved`, `ExternalArtistsUnresolved`,
    `ExternalSourceErrors{source}`. The unresolved *ratio* is the one worth alarming on — a
    sudden jump means an upstream shape change, not a slow day.

**Phase E — storage (`internal/store/`, `internal/model/`)**

18. `model.ArtistProfile` in `entities.go`, with `Member` as its own type.
19. `SKExternal = "EXTERNAL"` in `keys.go`; `artistProfileItem` in `items.go` with the same
    explicit `dynamodbav` tags and to/from-model functions as `artistItem`.
20. `PutArtistProfile` / `GetArtistProfiles(ctx, ids)` in `dimensions.go`, the getter batched
    like `GetArtists`. Add `ExternalStaleAfter = 180 * 24 * time.Hour` beside
    `DimensionStaleAfter`, with a comment saying why the two differ.
21. DynamoDB Local tests in `dimensions_ddb_test.go`, including the case that matters: a
    `META` row and an `EXTERNAL` row for the same artist are independent, and rewriting one
    never clobbers the other.

**Phase F — entry points**

22. `cmd/enrich/` — Lambda handler, mirroring `cmd/capture/`.
23. `cmd/spotistats/enrich.go` — extend the existing command with an `external` subcommand
    (`--limit`, `--force`, `--artist`), plus `spotistats mbid set|clear` for the §4.5.2
    manual override.
24. `cmd/spotistats/doctor.go` — add checks: is the TheAudioDB key configured, is the
    MusicBrainz contact string set, do both hosts answer.

**Phase G — infrastructure (`infra/`)**

25. Fourth Lambda in `stack.go`: `provided.al2023`, arm64, 512 MB, **5-minute timeout**,
    **`ReservedConcurrentExecutions: 1`** (§4.5.4), on a daily 04:15 UTC EventBridge rule.
26. SSM `SecureString` for the TheAudioDB key, read at cold start — same reasoning as §9.2.
27. **Add `https://r2.theaudiodb.com` to the CSP `img-src`** in `web.go`, and extend the
    existing `TestSecurityHeadersAllowSpotifyImages` to assert both hosts. Every fanart,
    banner and logo is blocked without it, and the failure mode is a silently image-less page.
28. Alarm on enrich-Lambda errors and on the unresolved ratio; add both to
    `TestAlarmsCoverEveryMetric`.

**Phase H — surfacing**

29. `GET /artists/{id}/profile` in the query API (§6.1) — one item read, returning the
    `EXTERNAL` row with its provenance block. Absent row → `404`, not an empty object, so the
    client distinguishes "never enriched" from "enriched and empty".
30. Artist profile UI (§7.7).

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
PK  ARTIST#{id}   SK  META    name, genres[], popularity, followers, imageUrl,
                                refreshedAt, enrichedAt
PK  ALBUM#{id}    SK  META    name, releaseDate, releaseDatePrecision, imageUrl,
                                totalTracks, artistIds, refreshedAt
PK  ARTIST#{id}   SK  EXTERNAL  mbid, mbGenres[], artistType, country, areaName,
                                beginAreaName, beganAt, beganPrecision, ended,
                                members[], biography, images{}, sources{},
                                refreshedAt            (§4.5)
```

`EXTERNAL` is deliberately a **second item** rather than more attributes on `META`: it holds
multi-kilobyte prose off the 50-at-a-time leaderboard read path, refreshes on a 180-day cycle
instead of 30, and can be half-populated by a partial source outage without endangering the
genre attribution `META` carries. See §4.5.5.

**Aggregate** — the query engine.
```
PK  AGG#{DIM}#{PERIOD}     DIM    ∈ TRACK | ARTIST | ALBUM | GENRE | TOTAL
SK  {entityId}             PERIOD ∈ ALL | 2025 | 2025-03 | 2025-03-14
    plays, playsExact, msPlayed, msPlayedExact, firstPlayedAt, lastPlayedAt
    dim, period, entityId          (denormalised copies of what the keys encode)
```
On the artist row, `refreshedAt` is when the row was last written and `enrichedAt` is when
the full `GET /v1/artists` object was last fetched. They differ because the name arrives free
with every play while genres need an API call, so a row can be current and never enriched;
`enrichedAt` absent means "never asked", which is not the same as "has no genres". See §4.1
step 5.

`imageUrl` on the artist and album rows is the widest image Spotify returned (§2.7). It is a
**refreshable cache, not an identifier** — nothing keys off it, and the enrichment pass
rewrites it. §7.6 adds a second `thumbUrl` field beside it for the thumbnail sizes the UI
actually renders.

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
PK  STATE   SK  EXTERNAL_ENRICH_CURSOR  resumable MusicBrainz/TheAudioDB position (§4.5)
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
| `/artists/{id}/profile` | — | **Built.** External enrichment: bio, members, origin, formed, photos, per-source provenance (§4.5), merged with the `META` label and the all-time artist aggregate so an unresolved artist still returns a listening block. Absent row is a `404`, not an empty object, so "never enriched" stays distinguishable from "enriched and empty". The id is a **path** parameter, not a query one — it addresses a single resource. |

`dim` ∈ `track|artist|album|genre`. `period` is `ALL`, `YYYY`, `YYYY-MM`, or
`YYYY-MM-DD`. `metric` ∈ `plays|ms|msExact`. `bucket` ∈ `day|month|year`.

### 6.2 Conventions

- Pagination is opaque-cursor. Two kinds exist, both opaque to the client, because
  "base64 of the `LastEvaluatedKey`" is not sufficient for either paginating endpoint:
  - **`/plays`** carries the last sort key returned and resumes strictly after it. A
    `LastEvaluatedKey` is scoped to one partition query, and a play range spans several UTC
    partitions, so it cannot be handed to the client directly. Resuming from "last timestamp
    plus one millisecond" would be simpler but wrong — two plays can share a millisecond, and
    one would be silently skipped — so the full sort key travels. Cost is O(n) for a full walk.
  - **`/list`** carries a position in a *computed* ordering. Ranking by listening time cannot
    be done by DynamoDB (it orders by key; the measure is an attribute), so the ranking is
    produced in the handler and no DynamoDB key means "resume from rank 50".

  Both carry a fingerprint of the query they belong to: replaying a cursor against changed
  parameters is a 400, not a silently wrong page.
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

`/meta` applies the same honesty to the **coverage window**. The `firstPlayedAt` and
`lastPlayedAt` aggregate attributes are best-effort at write time (§5.2), so out-of-order
ingestion — a backfill, a replay — can leave `firstPlayedAt` *later* than `lastPlayedAt`. That
was observed in practice against synthetic data seeded in random time order, and rendering
"coverage: 23 Jul to 11 Jul" as fact gives a client no way to tell it is nonsense. So `/meta`
corrects the window using the poll cursor (which only moves forward), never reports an
inverted range, and sets `coverage.approximate` with a note. The play and duration counters are
exact regardless; only the two bounds are affected, and the nightly reconcile makes them exact.

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

**Dashboard `/`** — **Built.** Reads `data/dashboard.json` only.

Reading order, top to bottom, and it is **recent-first on purpose**:

1. Hero figure: total listening time, all-time.
2. KPI row: total plays · distinct tracks · distinct artists · current daily streak.
3. **Calendar heatmap, trailing 12 months.**
4. Top artists and tracks **for the current year**.
5. Listening rhythm: by hour of day, by day of week.
6. Top artists, tracks, albums and genres, **all time**.
7. Footer: coverage window, last-updated, estimated-data disclosure, music-only note.

> With seventeen years imported, all-time totals are dominated by whatever was played most a
> decade ago and barely move month to month. Leading with them buries the part that actually
> changes, so the heatmap — the one chart showing the shape of the whole period at a glance —
> comes straight after the headline numbers, and all-time rankings go last.
> `web/src/App.test.tsx` asserts the order, because a reordering is exactly what gets undone by
> accident.

Artwork thumbnails are specified in §7.6 and not yet rendered.

**Durations are always shown twice**: the readable form and the same value in minutes
(`3d 11h` with `5,051m` beneath it). They answer different questions — one is how long it feels,
the other is what you can compare across rows, sum, or check against another tool. Picking one
per context would leave the dashboard inconsistent about which it meant. Under an hour the
readable form already *is* minutes, so it is not restated. `components/Duration.tsx`.

**Tooltips on the activity, hour-of-day and weekday charts** work on hover, on keyboard focus,
and on **click, which pins them**. A native `title` is not enough: it never appears on a touch
device, cannot be reached by keyboard, and its delay makes scanning a 365-cell heatmap tedious.
Pinned tooltips dismiss on a second click, on Escape, or on a click outside, so a pin is never a
trap. The `title` attributes remain as the no-JavaScript fallback.

**Table alignment.** Numeric columns centre BOTH header and value. They were right-aligned
values under left-aligned headers, which left each header floating away from the column it
named; tabular figures still line the digits up within the column.

**Explorer `/explore`** — query API. **Implemented.**
- Filter row (single row, above the results): entity-type toggle (tracks / artists / albums),
  period picker (all time · year · month), free-text search, sortable metric columns.
- **The year list is derived from `/meta`'s coverage window, never hardcoded.** It began as a
  literal floor of 2015, justified on the grounds that Spotify's API retains only ~50 plays. The
  GDPR import invalidated that the day it landed: history reached back to 2009 and six years of
  it were unreachable, with nothing on screen to suggest the CONTROL was the limit rather than
  the data. A stale floor hides real data; a low one only shows empty years.
- Table: rank · name · listening · plays · last played, with an in-cell magnitude bar so the
  shape of the distribution is visible rather than only the numbers.
- **All query state lives in the URL**, so every view is a shareable deep link, and the
  resolved query is echoed in prose ("artists matching "within" · 2026 · by listening time,
  most first").
- Drill-down: row → totals, a trend, and for tracks the full per-play log with estimated
  durations marked. The trend follows the selected period: a year gets **months**, all time gets
  **years**, spanning the entity's own first and last play rather than a guessed window. All time
  previously got a "pick a year" nag — a large empty band in a fixed-height panel, asking the
  reader to narrow a query they had deliberately widened. It renders **between the filter row and
  the table**, not after it: the table fills the viewport by design, so a panel below it opened
  entirely off-screen and selecting a row read as nothing having happened. It carries a close
  button and answers Escape.
- **One viewport, one scroll region.** Filter row + panel + list sum to exactly the window
  height, and the list is the only thing that scrolls — `Load more` sits at the end of its rows
  rather than beneath it. Three properties make that hold, and each replaced a wrong first
  attempt:
  - The panel's height is derived from the **viewport**, never its content. Content-derived was
    the original bug: a track's play log is far taller than an artist's figures, and the
    chart/table toggle changes it again, so opening the panel collapsed the list to almost
    nothing and toggling resized it under the reader's pointer.
  - The space below the list is **measured, not assumed**. Card and page padding live there. A
    hardcoded 52px allowance was wrong (the real block measured 89), and
    `scrollHeight - element.bottom` was worse: `scrollHeight` never reports less than the
    viewport, so on a page that fits it counts empty space as content and shrinks the list on
    every pass until it hits the floor.
  - Re-measurement is **triggered explicitly** on open and close. The page height does not
    change when a fixed panel opens above a fill-to-viewport list, by construction, so no resize
    event fires and a ResizeObserver alone never runs.
  On a window shorter than ~750px the list's floor wins and the page scrolls. That is the
  deliberate concession: something has to give, and a slit of a list is worse than a scrollbar.
- CSV export of the loaded rows.

Deliberate deviations from the original sketch, each with a reason:

| Sketched | Built | Why |
|---|---|---|
| Virtualised table | 50-row pages with cursor pagination | The API pages by cursor already, so virtualisation would add a dependency and a scroll-position bug class to render rows the API has not sent. Revisit if a page ever exceeds a few hundred rows. |
| `artist` column | Not present | `/list?dim=TRACK` returns no artist field; adding one means a per-row lookup or an API change. Tracked with the artwork work in §12.4, which needs the same field. |
| Separate metric selector | Sortable columns | Clicking the column being ranked by is the same choice with one fewer control, and it puts the sort indicator where the reader is already looking. |
| Genre dimension | Still omitted from the toggle | No longer because it is empty — §4.5.6 fills it — but because a genre is not an entity. It has no artwork, no Spotify link to satisfy §2.7, and no per-play log, so the drill-down panel that every other dimension opens has nothing to show. Adding it means designing that panel, not extending the toggle. |

Routing is a ~40-line path router rather than a router library: two routes, no nesting, no
parameters. Filter changes use `replaceState`, never `pushState` — a filter row that pushes per
keystroke turns Back into "undo one character" and strands the reader dozens of entries from
the page they arrived on. The URL stays copyable either way.

**Names always carry their context.** A title identifies nothing on its own — "Bleed Out",
"Legacy" and "Mad World" all belong to several artists — so every surface that shows a name
shows the surrounding names too:

| Dimension | Shown |
|---|---|
| Track | title, then `artist · album` beneath |
| Album | title, then `artist` beneath |
| Artist | name alone — there is no context to add |

The rule lives in **one** place, `store.ResolveLabels`, because the rollup and the query API both
need it and an earlier version had a copy each that drifted — which is how the dashboard ended
up showing bare album titles. For a collaboration the **primary** (first-credited) artist is
shown; listing every collaborator overflows the label on exactly the releases whose titles are
already long. `artistName` and `albumName` stay separate fields rather than a pre-joined string:
the renderer owns layout, and the CSV export needs its own columns.

Context wears a **text token**, never a series colour. Both fields are omitted when empty, so a
partially-enriched entity renders as a bare title rather than with a blank second line.

**Layout.** One widget per row at up to `104rem`. The two-up grid it replaced halved the width
available to charts whose entire job is comparing bar lengths, and this is a desktop-first
dashboard. Still a max-width rather than full bleed — past roughly that point the eye loses the
row it is reading on a wide monitor — and still responsive: the grid is single-column at every
size, so narrow screens are unaffected.

### 7.3 Visualization spec

Forms are chosen by the data's job, not by variety. Almost every question on this
dashboard is *"compare magnitude, low → high"*, which means **sequential single-hue,
not categorical**. Categorical colour is reserved for the rare case where distinct
series are genuinely the subject.

| Widget | Form | Colour job |
|---|---|---|
| Total listening time | Hero figure, ≥48px | text tokens only |
| Plays / tracks / artists / streak | KPI row of stat tiles | text tokens only |
| Top artists / tracks / albums | Horizontal ranked bars, full page width | sequential blue |
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

**Palette** (validated — see §7.5):

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

### 7.4 Local frontend development

The frontend must be runnable locally against a real backend so the edit-to-result loop is
seconds, not a deploy. Two modes, both driven by one environment variable:

```sh
cd web

# Mode A — against the deployed backend. Real data, no local AWS.
VITE_API_TARGET=https://stats.example.com npm run dev

# Mode B — fully offline. DynamoDB Local + the query API running locally.
VITE_API_TARGET=http://127.0.0.1:8787 npm run dev
```

`vite.config.ts` proxies `/api` to `VITE_API_TARGET`:

```ts
server: {
  proxy: {
    '/api': { target: process.env.VITE_API_TARGET, changeOrigin: true },
  },
},
```

**Why a dev proxy and not CORS on API Gateway.** The obvious alternative is to allow
`http://localhost:5173` as a CORS origin and have the browser call the API directly. That
was rejected:

- It would **permanently widen a public, unauthenticated API** to satisfy a development-only
  need. The API has no auth to protect, so the exposure is cost amplification (§10.3), and a
  browser-reachable dev origin is one more way to drive it.
- It adds a production configuration knob whose only consumer is a developer's laptop, so it
  is untested in production and easy to get wrong.
- With the proxy, the browser only ever talks to `localhost`; Vite makes the cross-origin hop
  **server-side**, where CORS does not apply. The result is that **no CORS configuration
  exists anywhere in the system**, which keeps the production same-origin design (§3.1)
  genuinely single-path rather than same-origin-plus-an-exception.

The proxy costs nothing at build time and disappears entirely in the production bundle, which
is served same-origin behind CloudFront.

**Mode B is available.** `spotistats serve` runs the query handlers as a plain HTTP server on
`127.0.0.1:8787` against DynamoDB Local, reusing the same handler the query Lambda wraps —
`internal/api` is a plain `http.Handler` and a test asserts the Lambda adapter produces
byte-equivalent responses, so the offline loop exercises production behaviour rather than an
approximation. Combined with `SPOTISTATS_DDB_ENDPOINT` and `SPOTISTATS_TOKEN_FILE` (§8.1), the
whole system — capture, storage, API, frontend — runs on a laptop with no AWS account:

```sh
make dev          # DynamoDB Local + table
make dev-seed     # synthetic data, so there is something to render
make serve        # query API on 127.0.0.1:8787
make web-dev      # Vite, proxying to it
```

**Static data files.** The dashboard reads `data/dashboard.json` from the same origin rather
than the API (§3.1). In Mode B `spotistats serve` also serves `/data/*` from the local
renderer output, so the dashboard works offline too.

### 7.5 Palette validation

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

### 7.6 Artwork in the UI — **built (milestone 8b)**

The data is already there (§2.7): every artist, album and track leaderboard entry in
`data/dashboard.json` can carry an `imageUrl`, and the CSP already allowlists `i.scdn.co`
(§9.1) with a test that fails if it is ever dropped. What is missing is the rendering, one
storage refinement, and the API field the Explorer will need.

**Where artwork goes, and where it does not.**

| Surface | Treatment |
|---|---|
| Top artists / albums / tracks (ranked bars) | Leading thumbnail, ~40px, before the rank label |
| Explorer table rows | Leading thumbnail, ~28px, in the name cell |
| Explorer drill-down header | One large image, ~160px |
| Genre mix | **No artwork** — genres are not entities and never will be (§2.7) |
| Hero figure, KPI row, calendar, rhythm | No artwork — these are measures, not entities |

> **Artwork is decoration; the name is the identity.** This is the §7.3 rule about colour
> applied to images: a row must remain fully legible with every image failed or blocked. The
> text label is therefore never replaced by a thumbnail, never truncated to make room for
> one, and the layout reserves the image box whether or not an image loads.

**One storage change first: capture a thumbnail size, not just the widest.** `widestImageURL`
keeps `images[0]`, which is the ~640px asset. A 100-row Explorer table would then pull ~100
full-size covers to render them at 28px — several MB of transfer to paint a few thousand
pixels — and the URL cannot be rewritten to a smaller size after the fact (§2.7). The fix
belongs at capture time, where the array is still in hand:

- Add `ThumbURL` beside `ImageURL` on `model.Artist` and `model.Album`, selected as **the
  narrowest image at least 160px wide**, falling back to the widest when the array offers
  nothing smaller. Two fields, not the whole array: the UI has exactly two jobs, thumbnail
  and hero, and storing five URLs to serve two is a schema paying rent for nothing.
- Persist it as `thumbUrl` on the `ARTIST#`/`ALBUM#` `META` rows (§5.2), carry it through
  `store.LeaderboardEntry` and the snapshot `Entry` exactly as `imageUrl` is carried today.
- Existing rows keep working: `thumbUrl` is absent until the row is next enriched, and the
  renderer falls back to `imageUrl`. No backfill, no migration.

**Then expose it on the query API.** `internal/api` currently returns no image field on any
response, so the Explorer has nothing to render even once the storage side is done. Adding
`imageUrl`/`thumbUrl` to the leaderboard and entity responses is the gating change for the
§7.2 Explorer artwork, and it is additive — no existing client field moves.

**Rendering rules.**

- **Fixed-size square box, `object-fit: cover`, `aspect-ratio: 1`.** Reserving the box keeps
  cumulative layout shift at zero when a row of thumbnails resolves at different times.
  Album art is square at source; artist images are not, so the crop is unavoidable there.
  Keep album covers uncropped and unfiltered — nothing is overlaid on artwork.
- **`loading="lazy"` and `decoding="async"`** on every thumbnail below the fold. Thirty
  images on the dashboard is fine eagerly; a hundred-row Explorer table is not.
- **`alt=""`** — the thumbnail is decorative because the entity name is adjacent text.
  Repeating the name in `alt` makes a screen reader announce it twice.
- **Failure and absence are the same case.** A tombstoned row (`Missing`), a never-enriched
  artist, and an `onerror` from the CDN all render the identical fallback: a neutral surface
  tile with the entity's first initial in a muted text token. Never a broken-image glyph,
  never a gap that changes the row height.
- **Never re-host or proxy the images.** Serving them from our own S3 would add storage and
  transfer cost, break Spotify's link-back framing, and pin a copy that goes stale the moment
  an artist changes their photo. Hotlink `i.scdn.co`, which is what the CSP entry is for.

**The link-back is mandatory, not decorative.** §2.7's policy obligation is discharged here:
every artwork and entity name links to that entity on Spotify. The URL is derivable from the
ID already stored — `https://open.spotify.com/{artist|album|track}/{id}` — so nothing new
needs capturing. The footer carries the "content supplied by Spotify" attribution once for
the page rather than repeating it per row.

A **name-keyed** entity (§4.2) has no Spotify identity yet, so there is nothing to link to and
`SpotifyLink` renders its children unwrapped rather than emitting an href that 404s.

#### As built

`components/Artwork.tsx` renders the image or the fallback tile; `SpotifyLink` wraps it.
Thumbnails appear on the dashboard cards (both bar rows and table view), the Explorer table,
and as a 160px inset on the drill-down header. Genres get none, having no Spotify entity.

In the Explorer table only the **artwork** leaves for Spotify; the name opens the drill-down. A
row click that navigated away would make the panel unreachable.

**Expect `thumbUrl` to be absent for a while.** It is captured, but only from the point the row
is next enriched — existing rows keep their `imageUrl` and the renderer falls back to it, exactly
as designed, so nothing breaks and no migration is needed. Until enrichment catches up the UI
does pull full-size assets to paint small boxes, which is the cost the field exists to remove.
**Do not attempt to synthesise a smaller URL from a larger one**: `i.scdn.co` paths do encode a
size prefix, and rewriting it is tempting and forbidden for the reason in §2.7 — it is an
unsupported URL that can 404 without warning.

**Operational note.** CloudFront caches API responses for an hour (`s-maxage=3600`), so a
query-Lambda change is invisible at the edge until it expires — which reads exactly like the new
field not working. `make push-lambda-query` now invalidates `/api/*` for that reason.

---

### 7.7 Artist profile — the surface for external enrichment (built)

§4.5's facts land on a **drill-down profile** at `/artist/{spotifyId}`, reached by clicking an
artist name on any leaderboard or from the Explorer's drill-down — not on the dashboard, which
stays a single-page summary of measures.

The artwork keeps its outward Spotify link while the **name** links inward. Both are required:
Spotify's Developer Policy makes the link-back a condition of showing the artwork (§2.7), and a
reader clicking a name wants the profile, not another tab.

| Block | Content | Source shown |
|---|---|---|
| Header | Fanart or wide thumb as a banner, artist thumb inset, name | TheAudioDB |
| Facts strip | Type · formed-in city · country · formed (at stored precision) · member count | MusicBrainz |
| Biography | `strBiography`, clamped to ~6 lines with an expand control | TheAudioDB |
| Members | Name · instruments · tenure, current members first, past members dimmed | MusicBrainz |
| Genres | Two clearly separated chip rows: **Spotify** and **MusicBrainz** | both |
| Listening | The stats Spotistats actually owns: plays, minutes, first/last played | Spotistats |

Rules that follow from §4.5 rather than from taste:

- **Never blend the two genre lists into one row of chips.** They are different taxonomies
  (§4.5.6), and a merged row implies an agreement that does not exist. Two labelled rows, or
  one row with the source on each chip.
- **Render date precision honestly.** `beganAt` may be `2008`, `2008-04` or `2008-04-17`. Claim
  exactly what is stored and no more — "2008", never "1 January 2008". This is the same rule the
  album release date already follows. See the refinement below: honouring the precision is not
  the same as printing the raw string.
- **A missing profile is a missing profile.** An unresolved artist (§4.5.2) shows the
  listening block alone, with a one-line "no external profile linked" note. Never a skeleton
  that implies data is loading, and never invented placeholder prose.
- **Provenance is visible, not buried.** Each block carries its source, and the page footer
  credits Spotify, MusicBrainz and TheAudioDB with links — TheAudioDB's terms require the
  credit and link-back explicitly (§4.5.7), and MusicBrainz genres are CC-BY-NC-SA, so the
  attribution is a licence obligation on the genre chips specifically, not a courtesy.
- **Logos are used as-is.** `strArtistLogo` is frequently a trademarked wordmark, which
  TheAudioDB's terms require be displayed unmodified — no recolouring, no CSS filters, no
  compositing it into another graphic. If it does not fit the layout, do not show it.
- Fanart is decorative and **`alt=""`**, same rule as §7.6; the artist name is adjacent text.
- The banner is the one place a large image is worth the bytes. Everywhere else on the page
  reuses the §7.6 thumbnail rules.

Settled by building it:

- **Three states, not two.** A `404` (never enriched), a tombstone (checked, no MusicBrainz
  link) and a resolved profile are three different pages with three different sentences. The
  tombstone is the one that justifies storing a negative result at all: it can say *when* it
  was checked, which "no profile yet" cannot.
- **Precision is rendered, not preserved verbatim.** `1996-04` displays as "April 1996", not as
  the string `1996-04`. That asserts exactly what MusicBrainz asserts — a month, no day — while
  still reading as a date. The verbatim form is a machine string, and a reader parses it as a
  broken date rather than as a precision claim. Member tenures are the exception: that column
  shows **years only**, because a tenure is a span and mixing `1996` with `22 February 2011`
  down one column makes the spans impossible to compare.
- **The banner box is TheAudioDB's own 1000×185.** Anything taller crops the sides, which on a
  band photo means cutting off band members and half the wordmark.
- **No per-source colour on the genre chips.** The row label carries the distinction — it has
  to, since colour cannot be the only signal — and a 3px left border on a fully rounded pill
  renders as a stray crescent rather than an accent.
- **Attribution follows `sources`, not the presence of the page.** A MusicBrainz hit with a
  TheAudioDB miss is the normal partial case (§4.5.5), so the footer credits only the services
  that actually answered. Claiming a biography came from a service that never replied is worse
  than omitting the credit.
- **`strArtistLogo` is not rendered anywhere.** It is frequently a trademarked wordmark that
  TheAudioDB's terms require be shown unmodified, and this layout has no slot where it would
  sit at its own size without recolouring or compositing. The rule above says "if it does not
  fit the layout, do not show it"; it does not fit.

---

## 8. Local CLI (`cmd/spotistats`)

A single Go binary using the operator's own AWS credentials directly against DynamoDB —
no privileged HTTP endpoint exists, so there is nothing internet-facing to abuse.

| Command | Purpose |
|---|---|
| `auth login` | **Built.** Runs the one-time OAuth flow: loopback listener on `http://127.0.0.1:8888/callback`, random `state` validated on the callback, code exchanged, granted scopes checked, refresh token written. |
| `auth status` | **Built.** Exercises a real token refresh and calls `recently-played` — a stored token Spotify has revoked is indistinguishable from a good one until it is used. |
| `config` | **Built.** Prints the resolved configuration with secrets redacted. |
| `init-table` | **Built.** Creates the table from `store.CreateTableInput` for local development. Refuses to run without `SPOTISTATS_DDB_ENDPOINT`, since in AWS the table is CDK's to own. |
| `backfill --path <dir>` | **Built.** Imports the unzipped GDPR export (§4.2). `--dry-run` parses and reports without writing and needs no AWS credentials; `--min-ms` (default 30000) sets the shortest counted stretch; `--enrich-only` / `--enrich-limit` run the resumable ID-resolution pass on its own. |
| `backfill-prune --from --to` | **Built.** Deletes API-sourced plays superseded by an imported export window, so the exact durations win (§4.2). Prompts unless `--yes`. |
| `enrich` | **Built.** Backfills `META` names and genres for artists already recorded. `--limit` (default 200), `--force`, `--timeout`. |
| `enrich-external` | **Built.** Resolves MusicBrainz + TheAudioDB facts into `ARTIST#/EXTERNAL` (§4.5). `--limit`, `--force`, `--artist`, `--timeout`. Resumable through `STATE / EXTERNAL_ENRICH_CURSOR`. |
| `mbid set\|clear <spotifyId> [mbid]` | **Built.** The manual escape hatch of §4.5.2 — the *only* way an MBID is ever assigned by judgement rather than by an asserted link. |
| `resolve` | **Built.** Upgrades placeholder track rows to real Spotify identity (§4.2.1). `--limit` (default 300) budgets the run against a quota shared with capture; `--dry-run` reports the backlog and spends nothing; a 429 ends the run cleanly and it is resumable. |
| `doctor` | **Built.** Diagnoses unresolved leaderboard names, reports the **name-keyed share per dimension** with the biggest offenders named (§4.2.1), and probes both enrichment sources and the Slack webhook. The first thing to run when the site shows an ID where a name belongs, or when a ranking looks wrong. |
| `rollup` | **Built.** Reconciles aggregates, refreshes leaderboards and renders the snapshots. `--window` days, `--all` for the entire history, `--no-render` to reconcile only. |
| `poll` | **Built.** Runs the capture pipeline (§4.1). `--dry-run` reports what would be ingested without writing; `--limit` overrides the page size. |
| `serve` | **Built.** Runs the query API on `127.0.0.1:8787` against DynamoDB Local, optionally serving `/data/*` and a built bundle, so the frontend dev server has a fully offline backend. See §7.4. |
| `dev-seed` | **Built.** Writes a synthetic dataset to a local table so the frontend can be developed before the export arrives. Deliberately awkward by construction — multi-artist tracks, artists with no genres, albumless tracks, a diurnal play distribution — so charts exercise the cases production will. Refuses to run without `SPOTISTATS_DDB_ENDPOINT`. |

`backfill` supports `--dry-run`; `backfill-prune` prompts for confirmation. The enrichment
commands have neither, deliberately: they are idempotent, resumable and additive — a
re-run costs API quota, never correctness — so a dry run would report only "I would ask
the same questions again".

### 8.1 Running without AWS

The refresh token normally lives in SSM, but the token store is an interface with a
file-backed implementation, so the whole pipeline runs with **no AWS account and no AWS
credentials**. The make targets wire it up:

```sh
make dev-env        # scaffold .dev/env (mode 0600), then fill in the two Spotify values
make dev            # DynamoDB Local + create the table
make auth-login     # one-time, opens a browser
make poll           # ingests real plays into DynamoDB Local
```

`.dev/` is gitignored, and `.dev/env` is where the Spotify client ID and secret live for local
runs. Without it the CLI falls back to SSM and needs AWS — the one thing this flow exists to
avoid — so `make dev` warns when the values are unset.

The equivalent raw configuration, if not using make:

```sh
export SPOTISTATS_DDB_ENDPOINT=http://localhost:8000        # DynamoDB Local
export SPOTISTATS_TOKEN_FILE=./.dev/refresh_token.json      # 0600, unencrypted
export SPOTISTATS_TABLE_NAME=spotistats
export SPOTISTATS_CLIENT_ID=...                             # else read from SSM
export SPOTISTATS_CLIENT_SECRET=...

spotistats init-table    # in AWS the table is CDK's; this is for DynamoDB Local
spotistats auth login
spotistats poll
```

`SPOTISTATS_DDB_ENDPOINT` also bypasses the AWS credential chain in favour of static
throwaway credentials, so an expired SSO session is irrelevant. The file-backed token store
is development-only: it is unencrypted at rest, which is why it is opt-in via an explicit
path rather than a fallback.

`SPOTISTATS_SPOTIFY_BASE_URL` and `SPOTISTATS_TOKEN_URL` point the client at a stand-in for
the Spotify API. They exist so the CLI can be exercised end to end in CI, where the real API
is unreachable and needs a human to authorise. Leave both unset in production.

### 8.2 Build and deploy

`make help` lists every target. The parts of the system and how each is built and shipped:

| Part | Build | Deploy |
|---|---|---|
| Operator CLI | `make build-cli` → `bin/spotistats` | runs locally, not deployed |
| Lambda binaries | `make build-lambdas`, or `build-lambda-<fn>` | `make deploy` (via CloudFormation) |
| Lambda code only | `make package-lambdas` → zips | `make push-lambdas`, or `push-lambda-<fn>` |
| Infrastructure | `make synth` | `make deploy` / `make diff` / `make destroy` |
| Frontend | `make build-web` | `make deploy-web` (S3 sync + CloudFront invalidation) |
| Data snapshots | rendered by the rollup Lambda | `make deploy-data` |

Two paths exist for Lambda code on purpose. `make deploy` goes through CloudFormation and
takes minutes; `make push-lambdas` calls `update-function-code` directly and takes seconds.
**`push-*` is correct only when handler code changed and nothing in `infra/` did** — a
configuration, permission or schedule change still needs `make deploy`, and pushing over a
stack whose template has drifted makes the next `cdk diff` misleading.

Adding a Lambda means adding it to the `LAMBDAS` variable in the `Makefile` and nowhere else:
every build, package and push target iterates over that list.

`make destroy` requires typing the stack name to confirm. The DynamoDB table has
`DeletionPolicy: Retain`, so listening history survives it — which is the point, since the
API cannot re-serve history.

---

## 9. Infrastructure (CDK, Go)

`github.com/aws/aws-cdk-go/awscdk/v2`. Two stacks (§3.1):

**`SpotistatsGlobalStack`** (`us-east-1`) — only what cannot live elsewhere:

| Resource | Configuration |
|---|---|
| ACM certificate | DNS-validated against the hosted zone; the sole reason this deployment spans two regions |
| AWS Budget | $10/month at 80% actual spend, published to the alarm topic so it lands in the same Slack channel. Lives in the REGIONAL stack beside that topic: budget ARNs carry no region, `AWS::Budgets::Budget` is in the eu-west-1 resource specification, and the global stack deploys first so a budget there could not reference a topic here |

**`SpotistatsStack`** (`eu-west-1`) — everything else:

| Resource | Configuration |
|---|---|
| DynamoDB `spotistats` | On-demand, PITR on, `GSI1`, `RemovalPolicy.RETAIN`, contributor insights off |
| `capture-lambda` | Go `provided.al2023`, **arm64**, 512 MB, 120s timeout. No reserved concurrency — see below |
| `rollup-lambda` | Go `provided.al2023`, arm64, 1024 MB, 900s timeout, reserved concurrency 1 |
| `query-lambda` | Go `provided.al2023`, arm64, 512 MB, 10s timeout, SnapStart n/a for Go |
| EventBridge rules | `rate(30 minutes)` → capture; `cron(15 3 * * ? *)` → rollup |
| SSM Parameters | `/spotistats/spotify/client_id`, `/client_secret`, `/refresh_token` — all `SecureString` |
| S3 `spotistats-web` | Private, versioned, encrypted, no public access, OAC-only |
| API Gateway HTTP API | Single `$default` stage, throttling (§10.3), access logs on |
| `query-lambda` | Go `provided.al2023`, arm64, 512 MB, 10s timeout, **read-only** DynamoDB. No reserved concurrency — see below |
| CloudFront | Two origins (S3 via OAC, API GW), HTTP/3, IPv6, compression, PriceClass 100 (NA+EU), SPA routing function, security-headers policy. Custom domain and TLSv1.2_2021 only when `domainName` is supplied |
| Route 53 | A + AAAA alias records → CloudFront. `spotistats.neovasili.com` is a **delegated** subdomain zone, so the domain *is* the zone and the records sit at its apex — `RecordName` must be omitted, or CDK appends the zone again and produces `spotistats.neovasili.com.spotistats.neovasili.com`. A test asserts the resulting name. |
| CloudWatch | Log groups (14-day retention), alarms and dashboard (§10.2) |

**Reserved concurrency is opt-in, and off by default.** The first deploy failed on it:

```
Specified ReservedConcurrentExecutions for function decreases account's
UnreservedConcurrentExecution below its minimum value of [10]
```

AWS requires at least **10 unreserved** concurrency to remain available account-wide. A new
account's `Concurrent executions` quota is **10**, so at that quota *any* reservation is
rejected — it is not a matter of reserving less. Both functions therefore deploy unreserved.

Nothing important is lost. Capture cannot overlap itself regardless: it runs every two hours
with a 120-second timeout and `RetryAttempts: 0` on the EventBridge target. The query function
is bounded by the API Gateway stage throttle (20 rps, 40 burst) and the budget alarm, which
§10.3 already identifies as the controls that matter given there is no WAF.

To re-enable after raising `Concurrent executions` in Service Quotas above roughly 20:

```sh
cdk deploy --all -c captureReservedConcurrency=1 -c queryReservedConcurrency=10
```

A test asserts the default stays unreserved, since restoring it unconditionally would make the
stack undeployable on a fresh account.

### 9.1 CloudFront behaviours

| Path pattern | Origin | Cache policy |
|---|---|---|
| `/api/*` | API Gateway | TTL 0/60/3600, forward query strings, no cookies |
| `/data/*` | S3 | TTL 0/300/86400 |
| `/assets/*` | S3 | TTL 1y, immutable |
| `/*` | S3 | TTL 0/60/300, SPA fallback |

**No CORS configuration exists on the API.** Production is same-origin behind CloudFront, and
local development uses a Vite proxy rather than a permitted browser origin (§7.4). If a CORS
header ever appears in this stack, something has gone wrong. A test asserts the synthesised
template contains none.

**SPA routing is a CloudFront Function, not custom error responses.** An earlier draft of this
section specified rewriting 403/404 from the S3 origin to `/index.html`, "scoped to the S3
behaviours only". **That is not implementable:** `CustomErrorResponses` is a
*distribution-level* setting with no per-behaviour form, so it would also rewrite a 404 from
the API into HTML — precisely the confusing failure the constraint was meant to prevent.

The correct mechanism is a **viewer-request function** attached to the site behaviour alone. It
runs before the cache lookup, costs a fraction of Lambda@Edge, and can discriminate on path:
`/api/*` and `/data/*` pass through untouched, anything whose last path segment contains a dot
is treated as a real asset (so a missing one 404s rather than silently rendering the app
shell), and everything else is rewritten to `/index.html`. The distribution sets no custom
error responses at all, and tests assert both that and that no other behaviour carries the
function.

**The bucket policy must be scoped by `AWS:SourceArn`.** An Origin Access Control grant
without it lets *any* CloudFront distribution in *any* AWS account read the bucket — the
classic OAC misconfiguration. A test asserts the condition names this distribution.

**The TLS policy only applies with a custom certificate.** On the default `*.cloudfront.net`
certificate CloudFront fixes the security policy, so setting `minimumProtocolVersion` there is
silently ignored; it is configured only alongside a custom domain.

Security headers via a CloudFront response-headers policy: HSTS (2y, includeSubdomains,
preload), `X-Content-Type-Options: nosniff`, `Referrer-Policy: strict-origin-when-cross-origin`,
`X-Frame-Options: DENY`, and a CSP of
`default-src 'self'; img-src 'self' https://i.scdn.co data:; style-src 'self' 'unsafe-inline'`.
`i.scdn.co` must be allowlisted or every album cover breaks — artwork is hotlinked from
Spotify's CDN rather than re-hosted (§7.6), so this entry is load-bearing, not optional.
**`https://r2.theaudiodb.com` joins it when §4.5 lands** — every fanart, banner and logo is
served from there, and the failure mode of forgetting it is a silently image-less profile
page rather than an error. One test asserts both hosts.

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

Alarms (all → SNS → a notifier Lambda → a Slack incoming webhook):

There is deliberately **no email subscriber**, and the reason is worth stating precisely because
the dramatic version of it is wrong.

The defect that actually shipped was a topic with **no subscriber at all**: `alarmEmail` was
unset, so both the email subscription and the budget skipped themselves in silence while ten
alarms sat in the console and three of them fired. Email was never the problem — once the
subscription existed and was confirmed it delivered normally, including a real
`ExternalEnrichFailed` alarm on 2026-08-24.

What a webhook buys is the removal of the *activation step*. An email subscription has two
states that the console renders identically: it is listed whether or not the recipient has
clicked the link, so "configured" and "will actually deliver" are indistinguishable from the
outside. A webhook has no such state, and the subscriber is created by this stack rather than
supplied as a value someone can forget — which is what closes the original gap for good. It
works on the first post or fails into `NotifyFailed`.

Every alarm also notifies on **recovery**. A channel that only ever says "broken" gives no way
to tell a live incident from a stale message.

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
| `NotifyFailed` | the Slack notifier errored, so some alarm did not arrive. **Not self-monitoring** — it travels through the function that failed — so check it in the console when the channel goes quiet |

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
| Lambda (12 captures + 1 rollup + 1 enrich daily + API) | ~$0.00 (free tier) |
| API Gateway HTTP API (~50k req) | ~$0.05 |
| S3 (< 1 GB, few thousand requests) | ~$0.03 |
| CloudFront (< 100 GB) | ~$0.00 (1 TB free tier) |
| Route 53 hosted zone | $0.50 |
| SSM Parameter Store (standard) | $0.00 |
| CloudWatch logs/alarms | ~$0.30 |
| **Total** | **~$1.05/month** |

The second region adds nothing material: an ACM certificate is free and a budget is free. The
`crossRegionReferences` machinery creates one SSM parameter and two custom-resource Lambdas
that run only during a deploy.

One-off backfill: ~$3–5 in DynamoDB write units for ~100k plays, plus a few hours of
wall-clock for metadata enrichment under development-mode rate limits.

**External enrichment (§4.5) adds no meaningful cost.** MusicBrainz is free and unauthenticated
and TheAudioDB's test key is free; the daily job is a few hundred requests inside the Lambda
free tier, and the `EXTERNAL` rows are a few KB each for a few thousand artists. What it costs
is *wall-clock*, not money — 1 req/s against MusicBrainz is the binding constraint, which is
why the job is budgeted and resumable rather than expected to finish in one run. A premium
TheAudioDB key (~$0 free / paid tiers for 100–120 req/min) only matters for the first
full pass; steady state is a handful of artists a day.

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
│   ├── enrich/              # enrich-lambda        (§4.5)
│   ├── notify/             # notify-lambda: SNS -> Slack (§10.2)
│   └── query/               # query-lambda
├── internal/
│   ├── httpx/               # shared retry/backoff/limiter  (§4.5); + httpxtest/
│   ├── spotify/             # API client: auth, retry/backoff, batching
│   ├── musicbrainz/         # MBID resolution, artist + members  (§4.5)
│   ├── theaudiodb/          # biography + artwork by MBID        (§4.5)
│   ├── enrich/              # external enrichment pipeline       (§4.5)
│   ├── notify/              # alarm -> Slack rendering            (§10.2)
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
| 3 | CLI + auth | **Done:** `auth login`, `auth status`, `poll` built; `poll` verified end to end against a fake Spotify + DynamoDB Local |
| 4 | Infra skeleton | **Done and deployed:** CDK stack synthesises with no credentials; table derived from `store.Schema` with a parity test; capture Lambda + 30-minute schedule + 8 alarms + budget |
| 5 | Backfill | **Done:** 408,613 plays imported from the GDPR export, 2009-11-01 → present, 26,381 hours with 99.96% exact durations. Artist/album identity is late-bound (§4.2), so full attribution works without waiting on a rate-limited API. |
| 6 | Query API | **Done and deployed:** all §6.1 endpoints implemented and tested against DynamoDB Local; `cmd/query` + API Gateway adapter verified to match direct serving; `spotistats serve` and `spotistats dev-seed` give the offline frontend loop (§7.4); S3 + HTTP API + CloudFront synthesise |
| 7 | Dashboard | **Done:** `internal/rollup` (reconcile, leaderboards, histograms, coverage, snapshots), `cmd/rollup` on a nightly schedule, and the React dashboard against the validated palette. Rendered output verified in a browser in both themes. |
| 8 | Explorer | **Done:** sortable table, dimension/period filters, search, cursor pagination, per-entity drill-down with a monthly trend and per-play log, CSV export, and URL-backed state so every query is a shareable deep link (§7.2). |
| 8b | Artwork | **Done:** `thumbUrl` captured beside `imageUrl`, image fields on the query API, thumbnails on the ranked bars and Explorer rows, initial-tile fallback, Spotify link-back and footer attribution (§7.6). |
| 8c | External enrichment | **Done and deployed (§4.5):** `internal/httpx` extracted (+ `httpxtest`); `musicbrainz` + `theaudiodb` clients against goldens captured from real responses; `internal/enrich` pipeline; `enrich` Lambda on a nightly 04:15 schedule, single-flighted by an expiring `STATE` lock rather than reserved concurrency (see §4.5.5); `ARTIST#/EXTERNAL` rows with a 180-day refresh; `GET /artists/{id}/profile`; artist profile page (§7.7). Exit criteria met: **zero name-matched resolutions** by construction — there is no name-search code path to disable — and the three profile states (resolved, tombstoned, never-enriched) were each verified against the deployed Lambda. Roughly two thirds of attempted artists resolve to an MBID; the rest are tombstoned and render a listening-only profile. |
| 9 | Hardening | **Done:** alarms delivered to Slack through a notifier Lambda (no email subscriber, so nothing waits on a confirmation click), $10 budget publishing to the same topic, PITR, security headers, 30-minute capture interval, and a 14-test Playwright smoke suite (`make smoke`) |
| 10 | CI/CD | **Done:** GitHub OIDC provider and a branch-scoped deploy role in CDK; `deploy.yml` runs checks → `cdk deploy` → publish → re-render → HTTP and browser smoke gates. **Manual trigger by default** — see below |
| 11 | Track identity | **Done and deployed; the pass itself drains over ~2 months.** `backfill.Resolver` shared by `spotistats resolve` and a nightly `resolve` Lambda (05:15 UTC, 200 tracks, store-driven work list, most-played first); a `RESOLVE_COOLDOWN` state row so a 429 stops the next run from asking at all; `doctor`'s name-keyed audit; `artistCoverage` corrected to count only rankable attribution; `ResolveStalled` and `ResolveFailed` alarms. Exit criteria: the name-keyed share reaches ~0% for ARTIST and ALBUM, at which point the attribution caveat disappears on its own and genre coverage rises from 56%. Rate-limited by design, not blocked — the constraint is Spotify's quota, which capture must keep priority on (§4.2.1). |

Milestone 1 gates everything and contains a step with up to 30 days of latency — start
it today. Milestones 2–4 do not depend on the export arriving; milestone 5 does.

**Why the deploy workflow is `workflow_dispatch` and not `push`.** This is a private
repository, so Actions minutes are billed, and the budget limit is already why `ci.yml`'s push
trigger is commented out. A deploy firing on every merge would spend that budget fastest of
all. The push trigger is present and commented in `deploy.yml`; uncommenting it is the only
change needed, because the OIDC trust policy already scopes the role to `refs/heads/main`.

**The deploy workflow does not repeat the DynamoDB integration suite.** It needs Docker and
takes minutes, and `ci.yml` already ran it against the same code on the pull request — `main`
is post-merge. It runs `go vet`, the pure Go suite and the frontend suite instead, then gates
the deploy on an HTTP check followed by the browser suite.

**The OIDC role holds almost no permissions of its own.** `cdk deploy` works by assuming the
roles CDK bootstrap created, so the GitHub role's main grant is `sts:AssumeRole` on those, plus
the few direct calls the deploy targets make outside CloudFormation (bucket sync, CDN
invalidation, invoking the rollup). `AdministratorAccess` would make one compromised workflow
run equal to an account takeover. `infra/stack_test.go` asserts both the branch scoping and the
absence of admin policies — a missing `sub` condition would leave the role assumable by any
repository on GitHub, which is the entire security boundary.

---

## 14. Open decisions

| # | Decision | Recommendation |
|---|---|---|
| 0 | Go module path | **Decided:** `github.com/neovasili/spotistats`. |
| 0b | `PLAY#` partition timezone | **Decided:** UTC, while every aggregate *period key* is local. The partition is storage addressing, not a semantic period, and decision 4 makes the timezone a runtime setting — local partitions would strand every existing row if it ever changed. A `STATE / CONFIG` row records the configured zone and schema version, and `store.VerifyConfig` turns a mismatch into a startup failure. Cost is one extra partition read, since a local month spans two UTC months. |
| 1 | Which domain / subdomain | **Decided:** `spotistats.neovasili.com`, account `401547103722`, region `eu-west-1`. Hosted zone `Z08622643JXD4FF65E2XP` is a **delegated** subdomain zone, so the domain is the zone and the alias records sit at its apex. Domain, zone and region are in `cdk.json`. The account is not: the CDK CLI resolves it from the active credentials, so hardcoding it would only add a second source of truth that could drift. |
| 2 | Capture cadence | **Decided: 30 minutes.** The plan said start at 2h and tighten if `PlaysGapDetected` fired; it was tightened during the milestone-9 hardening pass and the doc lagged. `recently-played` returns a rolling ~50 plays, so the interval has to stay comfortably shorter than the time it takes to play 50 tracks — about 2.5 hours of continuous listening. Note this cadence also spends the Spotify quota that track resolution competes for (§4.2.1). |
| 3 | Include skips as plays? | Match the API: count ≥30s only. Keep skipped rows in the export import so the definition can be revisited without re-importing. |
| 4 | Timezone for rhythm charts | `Europe/Madrid`, as an env var so it is changeable without a redeploy. |
| 4b | Frontend local-dev transport | **Decided:** Vite dev proxy, not CORS on API Gateway. Keeps the system free of any CORS configuration and avoids widening a public unauthenticated API for a development-only need. See §7.4. |
| 5 | Public or private repo | **Decided: private.** No secrets are in the code, so public would have been safe, but private means GitHub Actions minutes are billed — which is why both `ci.yml` and `deploy.yml` are `workflow_dispatch` only and the Playwright smoke suite is not wired into CI at all. The consequence worth stating plainly: **nothing runs automatically**, so `make lint test` locally is the real gate. |
| 6 | Podcast handling | Excluded (API cannot see them). State it in the UI footer. |
| 7 | Artwork resolution stored | **Recommendation:** store two URLs — the widest (hero) and the narrowest ≥160px (thumbnail) — rather than the whole `images` array or the widest alone. The URL cannot be resized after capture (§2.7), so keeping only the ~640px asset forces a 100-row table to pull ~640px covers for 28px boxes; keeping all five sizes stores three URLs the UI never asks for. See §7.6. |
| 8 | MusicBrainz genres in the genre aggregate | **Resolved: yes, adopted (§4.5.6).** The condition set here was "revisit once the resolution rate is known", with 90% called clearly worth it and 40% not. It landed at **56%** — squarely in the ambiguous middle — and was adopted anyway, because the comparison is not 56% against 90% but 56% against **zero**: Spotify's field is gone, so the alternative was a permanently empty chart. The two objections in §4.5.6 were answered rather than waived (nothing left to merge with; a full recompute paid, so no seam). The residual risk is ranking ORDER, which is measured and disclosed rather than assumed away. |
| 9 | TheAudioDB key tier | **Recommendation:** start on the free test key (`123`, 30 req/min). MusicBrainz's 1 req/s is the binding constraint on the first full pass anyway, so a paid tier buys nothing until that stops being true. Revisit only if the initial backfill proves too slow. |
| 10 | Biography language | **Recommendation:** English, configurable, with an English fallback. TheAudioDB returns 15 translations; storing all of them multiplies the item ~15× for prose the single-language dashboard never renders (§4.5.5). Record the stored language in `biographyLang` so switching later is a re-enrich, not a guess about what is in the row. |
