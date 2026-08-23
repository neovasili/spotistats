# Spotistats — Manual Prerequisites

Everything in this document must be done **by hand**, outside the codebase. It cannot
be automated because it involves third-party consent screens, email confirmations, DNS
you may not control programmatically, and secrets that must never enter git.

Work through the steps in order. **Step 1 has up to 30 days of latency — do it today**,
before anything else, then continue with the rest while you wait.

**Legend:** 🕐 = has a waiting period · 🔐 = produces a secret · 💰 = costs money

---

## Step 1 🕐 — Request your Extended Streaming History

This is the *only* way to obtain listening history older than your most recent ~50
plays. The Spotify Web API cannot provide it (see `SPECS.md` §2.1, §2.4). Everything in
milestone 5 is blocked on this file arriving, so request it first.

1. Go to <https://www.spotify.com/account/privacy/> and log in.
2. Scroll to **"Download your data"**.
3. You will see up to three separate checkboxes. Tick **"Extended streaming history"**.
   - Do **not** settle for "Account data" alone — that returns only ~30 days of
     streaming history and is useless for backfill.
   - Ticking both is fine and costs nothing.
4. Click **Request data**.
5. **Check your email and click the confirmation link.** The request is not submitted
   until you confirm it. This is the most commonly missed step — if nothing arrives in
   a week, this is almost certainly why.
6. Wait. Spotify states delivery can take **up to 30 days**; in practice it is often
   2–14 days for extended history. You will get a second email with a download link.

**When it arrives:**

- Download the zip and store it somewhere durable and backed up. This file is your only
  cold backup of your own listening history (`SPECS.md` §10.4).
- Do **not** commit it to git. Add `*.zip` and `my_spotify_data/` to `.gitignore`.
- Expected contents: `Streaming_History_Audio_YYYY-YYYY_N.json` files (~12 MB each) plus
  a PDF describing the fields. Verify with:

  ```sh
  unzip -l my_spotify_data.zip | head -30
  ```

- Sanity-check one record before importing:

  ```sh
  unzip -p my_spotify_data.zip 'Streaming_History_Audio_*_0.json' | head -c 2000 | jq '.[0]'
  ```

  You should see `ts`, `ms_played`, `master_metadata_track_name`, and
  `spotify_track_uri`. If `spotify_track_uri` is null on most rows, you were sent
  podcast/local-file data — check you requested the *extended* history.

**Verification:** you have a local zip containing `Streaming_History_Audio_*.json`
files with non-null `spotify_track_uri` values.

---

## Step 2 🔐 — Create the Spotify developer app

1. Go to <https://developer.spotify.com/dashboard> and log in with the **same Spotify
   account** whose stats you want to track.
2. Accept the Developer Terms of Service if prompted.
3. Click **Create app** and fill in:

   | Field | Value |
   |---|---|
   | App name | `Spotistats` |
   | App description | `Personal listening statistics dashboard` |
   | Redirect URI | `http://127.0.0.1:8888/callback` |
   | Which API/SDKs | **Web API** only |

4. **The redirect URI must be exactly `http://127.0.0.1:8888/callback`.**
   Spotify's rules are specific and strict:
   - `localhost` is **not allowed** as a redirect URI. Using
     `http://localhost:8888/callback` will be rejected.
   - Plain HTTP is permitted **only** for loopback addresses, using an explicit IP
     literal: `http://127.0.0.1:PORT` or `http://[::1]:PORT`.
   - It must match the value your CLI sends, character for character, including the
     trailing path.
5. Save, then open the app's **Settings** and copy:
   - **Client ID**
   - **Client secret** (click *View client secret*)

Store both in your password manager now. 🔐 Never commit them.

### What you are *not* getting

Your app is created after 2024-11-27, so these are permanently unavailable to it:
Audio Features, Audio Analysis, Recommendations, Related Artists, Get Featured
Playlists, Get Category's Playlists, 30-second preview URLs, and algorithmic/editorial
playlists. This is expected and the design accounts for it (`SPECS.md` §2.3). Do not
waste time trying to get access.

Spotify's **February 2026** change removed more, and this one is easy to mistake for a bug in
Spotistats: the **batch multi-get endpoints** (`GET /v1/artists`, `/v1/tracks`, `/v1/albums`)
now return **403 Forbidden** for Development Mode apps. The single-item endpoints
(`/v1/artists/{id}` and friends) still work, and Spotistats uses those. Also gone:
`followers` is always null and `popularity` is deprecated; `genres` is deprecated but still
returned, so the genre charts work today and may not forever.

### Quota mode

Leave the app in **development mode**. Extended quota mode requires a review process
intended for production multi-user apps and will not be granted for a personal dashboard.

Since February 2026, development mode requires a **Spotify Premium account**, allows **one
Client ID** per developer, and permits at most **five allowlisted users** (it was 25 before).

⚠️ **Add your own Spotify account to the app's allowlist**, under *Settings → User
Management* in the app dashboard. A user who is not allowlisted receives **403 Forbidden** on
user-scoped calls, which looks identical to the removed-endpoint 403 described above.

> **Troubleshooting:** if authorization returns 403 for your own account, open
> **User Management** in the app settings and add your own Spotify account name and
> email explicitly. The owner account is normally allowed implicitly, but adding it is
> harmless.

**Verification:** you have a Client ID and Client Secret, and the app's redirect URI
list contains `http://127.0.0.1:8888/callback`.

---

## Step 3 — Install the local toolchain

| Tool | Minimum | Install (macOS) | Check |
|---|---|---|---|
| Go | 1.26+ | `brew install go` | `go version` |
| Node.js | 20 LTS+ | `brew install node` | `node -v` |
| AWS CLI | v2 | `brew install awscli` | `aws --version` |
| AWS CDK CLI | v2 | `npm i -g aws-cdk` | `cdk --version` |
| jq | any | `brew install jq` | `jq --version` |

**Verification:** every `Check` command succeeds and reports at least the minimum
version.

---

## Step 4 💰 — Prepare the AWS account

### 4.1 Create a deploy identity

Do not use root credentials. From the IAM console:

1. **IAM → Users → Create user**, name `spotistats-deploy`, no console access.
2. Attach `AdministratorAccess` for the initial deploy.
   - Justification: CDK bootstrap and first deploy legitimately need broad permissions.
     Tighten to a scoped policy after the stack exists if you care to.
3. **Create access key** → *Command Line Interface* → copy the key pair. 🔐

### 4.2 Configure a named profile

```sh
aws configure --profile spotistats
# AWS Access Key ID:     <from 4.1>
# AWS Secret Access Key: <from 4.1>
# Default region name:   eu-west-1
# Default output format: json
```

**Use `eu-west-1`.** Data and compute live there. The deployment also touches `us-east-1` for
one stack, because CloudFront accepts an ACM certificate only from that region — but the CDK
app sets both regions itself, so the profile's default only needs to be the main one
(`SPECS.md` §3.1).

Verify:

```sh
aws sts get-caller-identity --profile spotistats
```

### 4.3 Bootstrap CDK

Once per account/region:

Both regions need it, because CDK stages assets per environment and the certificate stack
lives in `us-east-1`:

```sh
export AWS_PROFILE=spotistats
make bootstrap
```

That resolves the account and bootstraps `eu-west-1` and `us-east-1`, creating a `CDKToolkit`
stack in each (an S3 assets bucket, an ECR repo, and IAM roles). Skipping the `us-east-1`
bootstrap makes the certificate stack fail with a confusing asset error.

### 4.4 Optional: raise the Lambda concurrency quota

A new AWS account has a `Concurrent executions` quota of **10**. That is enough to run
Spotistats, but it makes *reserved* concurrency impossible: AWS requires at least 10 unreserved
concurrency to remain available, so at that quota any reservation is rejected. The stack
therefore deploys with none, and the first deploy will fail if you re-enable it prematurely.

If you want the per-function bounds back:

1. **Service Quotas → AWS Lambda → Concurrent executions → Request increase**. Ask for 100;
   it is granted automatically for most accounts.
2. Once granted, deploy with the reservations enabled:

   ```sh
   cdk deploy --all -c captureReservedConcurrency=1 -c queryReservedConcurrency=10
   ```

This is genuinely optional. `SPECS.md` §9 explains what already bounds each function without it.

### 4.5 Set a budget 💰

Cheap insurance against a runaway loop or public-API abuse, since there is no WAF
(`SPECS.md` §10.3).

1. **Billing and Cost Management → Budgets → Create budget**.
2. Template: **Monthly cost budget**. Amount: **$10**.
3. Alert at **80%** of actual spend, to your email.
4. Confirm the SNS subscription email if prompted.

**Verification:** `aws sts get-caller-identity` returns your account, the `CDKToolkit`
stacks exist in both `eu-west-1` and `us-east-1`, and a $10 budget is active.

---

## Step 5 🔐 — Store the Spotify credentials in SSM

Do this before the first deploy, so the Lambdas find their parameters on day one. The
refresh token is written in step 6.

```sh
export AWS_PROFILE=spotistats
export AWS_REGION=eu-west-1

aws ssm put-parameter --name /spotistats/spotify/client_id \
  --type SecureString --value 'YOUR_CLIENT_ID' --overwrite

aws ssm put-parameter --name /spotistats/spotify/client_secret \
  --type SecureString --value 'YOUR_CLIENT_SECRET' --overwrite
```

> Use single quotes and beware of shell history. Prefix each command with a space, or
> use `read -rs VALUE` and pass `--value "$VALUE"`, to keep secrets out of
> `~/.zsh_history`.

Verify (this prints the secret — do it in a private terminal):

```sh
aws ssm get-parameter --name /spotistats/spotify/client_id --with-decryption \
  --query Parameter.Value --output text
```

**Verification:** both parameters exist as `SecureString` and decrypt to the expected
values.

---

## Step 6 🔐 — Obtain the initial refresh token

This is the one-time human consent step. The CLI (`spotistats auth login`) automates it
once the code exists; the manual procedure below is the fallback and the reference for
what the CLI must do.

Access tokens last 1 hour, so the long-lived **refresh token** is what gets stored.

### 6.1 Build the authorization URL

```sh
CLIENT_ID='YOUR_CLIENT_ID'
REDIRECT='http://127.0.0.1:8888/callback'
SCOPES='user-read-recently-played user-top-read'
STATE=$(openssl rand -hex 16)

python3 - "$CLIENT_ID" "$REDIRECT" "$SCOPES" "$STATE" <<'PY'
import sys, urllib.parse
cid, redirect, scopes, state = sys.argv[1:5]
q = urllib.parse.urlencode({
    'client_id': cid, 'response_type': 'code', 'redirect_uri': redirect,
    'scope': scopes, 'state': state, 'show_dialog': 'true',
})
print('https://accounts.spotify.com/authorize?' + q)
PY
```

Only two scopes are needed: `user-read-recently-played` (play capture) and
`user-top-read` (Spotify's own top-items rankings). Request nothing else.

### 6.2 Authorize and capture the code

1. Start a listener so the redirect has somewhere to land:

   ```sh
   # in a second terminal
   printf 'HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n\r\nAuthorized. Close this tab.\n' \
     | nc -l 127.0.0.1 8888
   ```

2. Open the URL from 6.1 in a browser, log in, and click **Agree**.
3. The browser is redirected to `http://127.0.0.1:8888/callback?code=…&state=…`. The
   `nc` terminal prints the raw request line — copy the `code` value out of it.
   - Confirm the returned `state` matches the `STATE` you generated. A mismatch means
     you should abort and start over.
   - The code expires in about a minute, so move to 6.3 promptly.
   - If `nc -l` misbehaves, just read the `code` from the browser's address bar; the
     connection failing is irrelevant once the code is in the URL.

### 6.3 Exchange the code for a refresh token

```sh
CODE='PASTE_THE_CODE'
CLIENT_ID='YOUR_CLIENT_ID'
CLIENT_SECRET='YOUR_CLIENT_SECRET'
BASIC=$(printf '%s:%s' "$CLIENT_ID" "$CLIENT_SECRET" | base64 | tr -d '\n')

curl -sS -X POST https://accounts.spotify.com/api/token \
  -H "Authorization: Basic $BASIC" \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  -d grant_type=authorization_code \
  -d "code=$CODE" \
  --data-urlencode 'redirect_uri=http://127.0.0.1:8888/callback' | jq .
```

`redirect_uri` here must be byte-identical to the one registered in step 2 — it is
validated, not merely echoed.

Response:

```json
{
  "access_token": "BQD…",
  "token_type": "Bearer",
  "scope": "user-read-recently-played user-top-read",
  "expires_in": 3600,
  "refresh_token": "AQC…"
}
```

### 6.4 Store the refresh token

```sh
aws ssm put-parameter --name /spotistats/spotify/refresh_token \
  --type SecureString --value 'AQC…' --overwrite
```

### 6.5 Confirm it works

```sh
BASIC=$(printf '%s:%s' "$CLIENT_ID" "$CLIENT_SECRET" | base64 | tr -d '\n')
ACCESS=$(curl -sS -X POST https://accounts.spotify.com/api/token \
  -H "Authorization: Basic $BASIC" \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  -d grant_type=refresh_token -d "refresh_token=AQC…" | jq -r .access_token)

curl -sS -H "Authorization: Bearer $ACCESS" \
  'https://api.spotify.com/v1/me/player/recently-played?limit=5' \
  | jq '.items[] | {played_at, track: .track.name}'
```

You should see your five most recent tracks with timestamps.

> **Note:** the response has no listening-duration field. That is expected, not a bug —
> `recently-played` does not expose `ms_played`, which is why API-era minutes are
> estimated (`SPECS.md` §2.2).

**Keep the refresh token in your password manager too.** If SSM is wiped you would
otherwise have to repeat this whole step. Refresh tokens do not expire but can be
revoked — if you ever revoke app access from your Spotify account, redo step 6.

**Verification:** `/spotistats/spotify/refresh_token` exists in SSM, and the curl in
6.5 returns real track names.

---

## Step 7 💰 — Choose the domain and prepare DNS

> **✅ Done for this deployment.** The domain is `spotistats.neovasili.com`, served from a
> **delegated hosted zone** `Z08622643JXD4FF65E2XP` in account `401547103722` — path B below.
> Both are recorded in `cdk.json`, so `make deploy` needs no extra arguments. The rest of this
> step is kept as reference for changing the domain later.

Pick a subdomain of a domain you already own. Which path applies depends on where its DNS is
hosted.

### Path A — DNS already in Route 53 (simplest)

Nothing to do manually. Note the parent hosted zone ID; CDK will create the records:

```sh
aws route53 list-hosted-zones --query 'HostedZones[].{Name:Name,Id:Id}' --output table
```

### Path B — DNS elsewhere, delegate the subdomain to Route 53 (recommended)

Gives CDK full control of the subdomain without moving your apex domain. Costs $0.50/mo
for the hosted zone.

1. Create a hosted zone for the **subdomain only**:

   ```sh
   aws route53 create-hosted-zone \
     --name stats.example.com \
     --caller-reference "spotistats-$(date +%s)" \
     --query 'DelegationSet.NameServers' --output table
   ```

2. Copy the four returned nameservers.
3. At your current DNS provider, on the **parent** zone (`example.com`), create an
   **NS** record:
   - Name/host: `stats`
   - Type: `NS`
   - Value: the four nameservers, one per line
   - TTL: 3600
4. Verify delegation (allow for TTL propagation):

   ```sh
   dig +short NS stats.example.com
   ```

   It must return the four AWS nameservers.

### Path C — Keep DNS entirely external (cheapest)

Avoids the $0.50/mo hosted zone, at the cost of two manual DNS records and CDK not
managing DNS.

1. Deploy the stack without DNS records. CDK outputs the ACM certificate's DNS
   validation `CNAME` and the CloudFront distribution domain.
2. At your provider, add the ACM validation `CNAME` exactly as given. Wait for the
   certificate to reach **Issued**:

   ```sh
   aws acm describe-certificate --certificate-arn <arn> \
     --query 'Certificate.Status' --output text
   ```

3. Add a `CNAME` for `stats` → `dxxxxxxxxxxxxx.cloudfront.net`.

> An apex domain cannot be a `CNAME`. If you ever want the apex rather than a
> subdomain, you need Route 53 alias records — i.e. Path A or B.

**Certificate region:** the ACM certificate **must** be issued in `us-east-1` regardless of
which path you choose — CloudFront ignores certificates from any other region. The CDK app
handles this: `SpotistatsGlobalStack` provisions it in `us-east-1` while everything else is in
`eu-west-1`, and its ARN crosses regions automatically.

**Verification:** Path A — you have the hosted zone ID. Path B — `dig NS` returns AWS
nameservers. Path C — you know where to add two DNS records post-deploy.

---

## Step 8 — Optional: GitHub Actions deployment via OIDC

Skip if you will only deploy from your laptop. This avoids storing long-lived AWS keys
in GitHub.

1. Create the OIDC identity provider (once per account):

   ```sh
   aws iam create-open-id-connect-provider \
     --url https://token.actions.githubusercontent.com \
     --client-id-list sts.amazonaws.com \
     --thumbprint-list 6938fd4d98bab03faadb97b34396831e3780aea1
   ```

2. Create a role `spotistats-github-deploy` with this trust policy, substituting your
   account ID and `owner/repo`:

   ```json
   {
     "Version": "2012-10-17",
     "Statement": [{
       "Effect": "Allow",
       "Principal": { "Federated": "arn:aws:iam::ACCOUNT_ID:oidc-provider/token.actions.githubusercontent.com" },
       "Action": "sts:AssumeRoleWithWebIdentity",
       "Condition": {
         "StringEquals": { "token.actions.githubusercontent.com:aud": "sts.amazonaws.com" },
         "StringLike": { "token.actions.githubusercontent.com:sub": "repo:OWNER/REPO:ref:refs/heads/main" }
       }
     }]
   }
   ```

   The `sub` condition scopes the role to your repo's `main` branch. Without it, any
   GitHub repository on the internet could assume the role — do not use `*` here.

3. Attach `AdministratorAccess` initially; scope it down later.
4. Add the role ARN as a GitHub repository variable `AWS_ROLE_ARN`.

**Verification:** the role exists and its trust policy names your repo and branch.

---

## Step 9 — Decide the remaining open questions

These are inputs the code needs; none require external setup.

| Question | Where it lands | Default |
|---|---|---|
| Subdomain to use | CDK context | — (step 7) |
| Timezone for rhythm charts | Lambda env var | `Europe/Madrid` |
| Capture cadence | EventBridge rule | every 30 minutes |
| Minimum ms to count as a play | CLI flag / rollup config | 30000 |
| Alarm notification email | CDK context `alarmEmail` | — (**see below**) |
| Repo public or private | GitHub | public is fine, no secrets in code |

### ⚠️ Set `alarmEmail`, or the monitoring is decorative

This one is not optional in practice, and it was missed on the first deployment with a real
consequence: the stack created **eight CloudWatch alarms, three of them firing, with zero
subscribers on the SNS topic** — and skipped the monthly budget entirely. The console showed a
monitored system that could not notify anyone.

Both the email subscription and the budget are gated on this single value. Add it to
`cdk.json`:

```json
{
  "context": {
    "alarmEmail": "you@example.com"
  }
}
```

Then `make deploy` and **confirm the AWS SNS subscription email** — an unconfirmed subscription
delivers nothing. `cdk synth` now prints a prominent WARNING whenever this is unset, so the
gap cannot recur silently, but the warning is not a substitute for setting it.

Optionally set `monthlyBudgetUsd` too (defaults to 10). The budget is one of the compensating
controls for running without a WAF, so it matters more here than the small figure suggests.

---

## Completion checklist

| # | Item | Blocking |
|---|---|---|
| ☐ 1 | Extended streaming history **requested** and email **confirmed** | milestone 5 |
| ☐ 1b | Zip downloaded and stored safely | milestone 5 |
| ☐ 2 | Spotify app created, redirect URI `http://127.0.0.1:8888/callback` | all |
| ☐ 2b | Client ID + secret in password manager | all |
| ☐ 3 | Go, Node, AWS CLI, CDK, jq installed | all |
| ☐ 4 | AWS profile `spotistats`, region `eu-west-1`, CDK bootstrapped in **both** `eu-west-1` and `us-east-1` | all |
| ☐ 4b | $10 budget alarm active | — |
| ☐ 5 | `client_id` + `client_secret` in SSM | first deploy |
| ☐ 6 | `refresh_token` in SSM, verified against the API | capture |
| ☑ 7 | Subdomain chosen (`spotistats.neovasili.com`), delegated zone provisioned | done |
| ☐ 8 | *(optional)* GitHub OIDC role | CI/CD |
| ☐ 9 | Open questions answered | as needed |

Items 2–5 and 7 unblock milestones 2–4, 6 and 7, so you can build essentially the whole
system while step 1 is still in the post. Only the historical backfill waits.

---

## Secrets hygiene

Three secrets exist. None of them belong in git:

| Secret | Home | Backup |
|---|---|---|
| Spotify client ID | SSM `SecureString` | password manager |
| Spotify client secret | SSM `SecureString` | password manager |
| Spotify refresh token | SSM `SecureString` | password manager |
| AWS access keys | `~/.aws/credentials` | password manager |

Add to `.gitignore` before the first commit:

```
*.zip
my_spotify_data/
.env
.env.*
cdk.out/
node_modules/
dist/
*.tfstate
```

If a secret is ever exposed: rotate the Spotify client secret from the developer
dashboard (which invalidates existing refresh tokens, so redo step 6), and deactivate
leaked AWS keys in IAM immediately.
