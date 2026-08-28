# Security Policy

Spotistats is a personal, single-user project maintained in spare time. There is no bounty
programme and no service-level commitment, but security reports are genuinely welcome and
will be looked at.

## Reporting a vulnerability

Please **do not open a public issue** for anything exploitable.

Use GitHub's [private vulnerability
reporting](https://github.com/neovasili/spotistats/security/advisories/new) — Security tab →
Report a vulnerability. That keeps the report private until there is a fix.

Useful things to include: what you did, what happened, and why it matters. A proof of
concept helps but is not required.

Expect a first response within about a week. Fixes ship when they are ready; there is only
one maintainer.

## Scope

In scope: this repository's source, the CDK infrastructure definitions in `infra/`, and the
deployment at `spotistats.neovasili.com`.

Out of scope, because they are known and intentional:

- **The site serves public data.** Everything the dashboard and Explorer return is meant to
  be world-readable. There is no viewer authentication by design, so "unauthenticated users
  can read the data" is not a finding.
- **No WAF.** Deliberate, documented in `docs/SPECS.md` §10.3, with API Gateway stage
  throttling and a billing alarm as the compensating controls.
- Reports from automated scanners with no demonstrated impact.
- Denial of service. Please do not test it — the compensating control is a budget alarm on
  someone's personal credit card.

## Secrets

Credentials are hand-created SSM SecureStrings, read at runtime, and never written into a
CloudFormation template. If you believe you have found a credential committed anywhere in
this repository or its history, please report it privately using the link above.
