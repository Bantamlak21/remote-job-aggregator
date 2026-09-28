# Accounts and saved jobs

Optional. Off unless `PUBLIC_BASE_URL` is set. With it off, the API is exactly the public,
read-only job API and none of the `/auth` or `/me` routes exist.

A visitor signs in with an email address, without a password: the site emails a single-use
link. Signed-in people can save jobs (bookmarks) and see them on a Saved page. Endpoints are in
[api.md](api.md#accounts-and-saved-jobs).

## Design

| Piece | Decision | Why |
|---|---|---|
| Sign-in | Emailed link, 10 minutes, single use | Nothing to leak or reuse; no password to store |
| Link secret | 256 random bits. Only its SHA-256 is stored | A database leak does not give working links |
| Link URL | `<PUBLIC_BASE_URL>/auth/verify#token=...` (fragment) | A fragment is never sent to a server, so not in access logs, proxy logs or `Referer` |
| Spending a link | The page calls `POST /auth/inspect` (shows the masked address, spends nothing), then `POST /auth/verify` only when the person presses "Confirm" | A mail scanner or link previewer that opens the link cannot sign anyone in |
| Single use | One `UPDATE ... WHERE used_at IS NULL AND expires_at > now RETURNING`, in the same transaction that opens the session | Exactly one of any number of racing requests wins; a failure to open the session does not burn the link |
| Session | Opaque 256-bit token in an HttpOnly cookie; only its SHA-256 is stored | Revocable server side; not readable by page scripts |
| Session life | 14 days idle (sliding on use, and the cookie itself is reissued when it slides), 30 days absolute from when it was opened | A stolen cookie stops working, and a daily user is not silently signed out |
| Session count | At most 10 live sessions per account; signing in again ends the oldest beyond that | Bounds how many stolen-cookie windows can exist at once |
| Signing in ends the caller's old session | `POST /auth/verify` ends whatever session the request already carried, in the same transaction that opens the new one | A session cannot be carried across a sign-in (session fixation); an old, possibly compromised session stops working the moment the real owner signs in again |
| Cookie | `__Host-session` (Secure, Path=/, no Domain) over HTTPS; `session` for plain-http development. SameSite Lax or Strict only — `None` is not offered, so a cross-site deployment is not supported | The `__Host-` prefix stops a sibling subdomain from planting a cookie; `SameSite=None` needs a browser to honor third-party cookies, which is not a safe assumption to build on |
| CSRF | Exactly one `Origin` header, equal to `CORS_ALLOWED_ORIGIN` (a missing Origin is refused, not allowed), and the exact media type `application/json` or a non-empty `X-Requested-With`, on every non-GET request to `/auth` and `/me` | A cross-site page cannot send either without a CORS preflight, which only the web app's origin passes; a real browser always sends `Origin` same-origin, so requiring it costs nothing legitimate |
| CORS | One exact origin (must equal `PUBLIC_BASE_URL`), credentials allowed for it only, never `*` | Startup refuses anything else |
| Account creation | On first successful sign-in. No separate sign-up | No account enumeration: `/auth/login` answers the same for new and known addresses |
| Email | ASCII only, one `@`, RFC 5322 atext local part, ASCII domain with a dot. Lower-cased. The database's own `CHECK` constraint refuses non-ASCII too, so a bug in the application check is not the only guard | What is stored and mailed to is exactly what was typed; no header or SMTP injection |
| Rate-limit key | The address with its aliases folded (`throttle_key`): dots and a `+tag` dropped for Gmail/Googlemail, a `+tag` dropped everywhere else | `a.b+1@gmail.com`, `ab@gmail.com` and `a.b@googlemail.com` share one budget instead of each getting a full one; the address that is stored and mailed to is untouched |
| Rate limits | Per address (3 per 15 minutes, 10 per day), per client IP (10 per 15 minutes, 50 per day), `AUTH_MAX_LINKS_PER_HOUR` links site-wide per hour (default 300), 30 link opens per IP per 15 minutes. Per-address and the site-wide count are also checked in the database, atomically (one advisory lock per throttle key covers both the count and the insert), so two server processes cannot together exceed either limit; the in-memory limiters cannot be starved by a flood of new keys (an existing entry is evicted, never a hard refusal that could lock out a legitimate address) | Bounds email bombing, sender reputation damage and token guessing, and survives a restart (site-wide count) or several instances (both counts) |
| Site-wide breaker | Crossing `AUTH_MAX_LINKS_PER_HOUR` logs `mail_breaker_open` at `WARN`, rate-limited to once a minute | Something to alert on (D.8) that a single address's own limit does not raise |
| Mail sending | A bounded queue with two workers. A full queue answers `503`. Each send is bounded to 10 seconds and a cancelled context (shutdown) closes the connection rather than waiting out a stalled server | A slow or hung SMTP server cannot pile up requests or block shutdown |
| Token check | A malformed token (wrong length or alphabet) and an unknown one take the same code path (the input is hashed and looked up either way) | Nothing about response time distinguishes "not a token" from "a token that doesn't exist" |
| Bookmarks | Up to 500 per account, saved under a lock on the account row. Only a job currently visible to the public (open, not past its deadline, within the age limit) can be newly saved. Saved jobs stay in the list after they stop being visible, marked `closed` | The limit cannot be beaten by concurrent requests, a save cannot be used to read a job that is not otherwise visible, and a saved job never silently disappears |
| Detail of a closed (no-longer-listed) job | `GET /jobs/{id}` shows it to the account that saved it while it was visible, `404` to everyone else, including another signed-in account that never saved it | The Saved page can always open what it lists, and nothing else |
| Deleting | `DELETE /me` (body `{"confirm": true}`; anything else is refused) removes the account, sessions, saved jobs and unused links at once | Immediate, no grace period, and not triggered by an empty or malformed body |
| Client address | Read only from a configured, explicitly allow-listed header (`X-Real-IP`, `X-Forwarded-For`, `CF-Connecting-IP`, `True-Client-IP`, `Fly-Client-IP`); for `X-Forwarded-For` the **rightmost** entry is used | The rightmost entry is the one a well-behaved proxy appended; the leftmost is whatever the client sent and lets it choose its own rate-limit key |
| Response headers | Every response carries `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`, `Content-Security-Policy: frame-ancestors 'none'`, `X-Frame-Options: DENY`; `/auth` and `/me` also always carry `Cache-Control: no-store` (success, error, 404, 405, preflight alike) | Defense in depth; nothing here is meant to be embedded in a frame or leak a Referer |
| A panic in a handler | Recovered, logged with the stack and the request id, answered as a generic `500` | A bug in one request cannot take the whole process down or leak an internal detail |
| Error responses | `{"error": "text", "code": "stable_identifier"}` on `/auth` and `/me` (see api.md) | A client branches on `code`, never on the text, which may be reworded |
| Unknown JSON fields | Refused (`400 bad_request`), not silently accepted and ignored | Deliberate, not the more common "additive-safe" choice: an unrecognized field in a request to a security-sensitive endpoint is more likely a client bug or a probe than a forward-compatible extra field, and it is easy to relax later if a real need for it shows up. If the SPA ever needs to send an extra field these routes don't expect, add it here explicitly rather than removing the check |

## Configuration

The variables are in the README table and `.env.example`. Startup fails, with a specific
message, on any of these:

- `PUBLIC_BASE_URL` that is not a bare origin (a path, query, fragment or credentials), or that
  is not `https` in production or staging. The value is normalized to the origin a browser sends
  (lower-case host, default port dropped).
- `PUBLIC_BASE_URL` left blank in production or staging: that is treated as a likely typo and
  refused, not as "sign-in is off". Set `AUTH_ENABLED=false` explicitly to run without sign-in
  there.
- `CORS_ALLOWED_ORIGIN` that is not equal to `PUBLIC_BASE_URL` (including `*`).
- `AUTH_MAILER=log` in production or staging, or with any `https://` `PUBLIC_BASE_URL` even in
  development (a forgotten `APP_ENV` must not be enough to print a live sign-in link) — the log
  would hold a link that works.
- `AUTH_MAILER=smtp` without `SMTP_HOST` and `SMTP_FROM`; outside development it also needs
  `SMTP_USERNAME` and `SMTP_PASSWORD` (without them every send fails at runtime while
  `/auth/login` still answers `202`, so sign-in looks fine and never delivers).
- `SMTP_TLS=none` in production or staging.
- `COOKIE_SECURE=false` in production or staging.
- `COOKIE_SAMESITE` set to anything but `lax` or `strict` (`none` is not offered: see the design
  table above).
- `CLIENT_IP_HEADER` set to anything other than `X-Real-IP`, `X-Forwarded-For`,
  `CF-Connecting-IP`, `True-Client-IP` or `Fly-Client-IP`.
- Sign-in on with `JOB_REPOSITORY=mock` (accounts need Postgres).

`AUTH_MAX_LINKS_PER_HOUR` (default 300) sets the site-wide sign-in link cap; see the design
table. `AUTH_ENABLED` is the explicit on/off switch described above; leave it unset to let
`PUBLIC_BASE_URL` decide.

Apply migration 000007 first: `aggregator migrate-up` (adds `users`, `login_tokens`, `sessions`,
`bookmarks`; `migrate-down --yes` removes them and every account with them).

### Development

```bash
PUBLIC_BASE_URL=http://localhost:5173 AUTH_MAILER=log COOKIE_SECURE=false aggregator serve
```

The sign-in link (a live, working token) and the full email address are printed in the
server log. Never do this with real users, and never point this log output at a shared,
retained or centrally shipped log stream — it is a local development convenience, not a
mailer.

## Deployment

**Serve the web app and the API from one site**, for example `https://jobs.example.com/` for the
app and `https://jobs.example.com/api/` proxied to the API (the worked examples below do this).
Then the cookie is first-party and SameSite Lax works, with nothing to configure beyond
`PUBLIC_BASE_URL`. A different site for the app and the API (`app.example.com` and
`api.other.com`) is not supported: that needs `SameSite=None`, which this API does not offer
(browsers with strict third-party cookie settings would silently drop the cookie anyway, so it
would not have worked reliably even if built).

A reverse proxy in front of the API must:

- terminate TLS, redirect http to https, and send `Strict-Transport-Security`;
- run as the **one and only** layer that sets the client-address header: the API trusts it
  completely, so a second hop (a CDN in front of the proxy, for instance) must either be the
  thing that sets the header or must itself be trusted to pass through only a value it verified;
- overwrite (nginx: `X-Real-IP`) or append to (a header the app reads by its **rightmost**
  value: `X-Forwarded-For`) the real client address, and set the matching `CLIENT_IP_HEADER`.
  Without it every visitor shares the proxy's address and the per-IP limits hit everyone at
  once. With it set and no proxy in the chain that reliably sets or appends it, a client picks
  its own limit key, so never set it otherwise;
- not log request bodies. The link token is in the fragment, so it is not in URLs the proxy sees,
  but `/auth/verify` and `/auth/inspect` bodies carry it.

**nginx**, one trusted proxy layer, `X-Real-IP` (simplest: nginx always sets it to the true peer,
so there is nothing to fold):

```nginx
server {
    listen 443 ssl;
    server_name jobs.example.com;
    # ssl_certificate ...; ssl_certificate_key ...;
    add_header Strict-Transport-Security "max-age=63072000; includeSubDomains" always;

    location /api/ {
        proxy_pass http://127.0.0.1:8080/api/;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header Host $host;
        # No proxy_set_header X-Forwarded-For here: CLIENT_IP_HEADER=X-Real-IP, and only
        # X-Real-IP is meant to reach the app.
    }
    location / {
        root /var/www/jobs-web/dist;
        try_files $uri /index.html;  # the SPA's own router handles the rest
    }
}
server {
    listen 80;
    server_name jobs.example.com;
    return 301 https://$host$request_uri;
}
```

Set `CLIENT_IP_HEADER=X-Real-IP`.

**Caddy**, the same layout (`X-Forwarded-For` is Caddy's default and the app reads its
rightmost entry, which Caddy itself appended):

```caddyfile
jobs.example.com {
    handle /api/* {
        reverse_proxy 127.0.0.1:8080
    }
    handle {
        root * /var/www/jobs-web/dist
        try_files {path} /index.html
        file_server
    }
}
```

Set `CLIENT_IP_HEADER=X-Forwarded-For`. Caddy sets HSTS and redirects http to https by default
over its automatic TLS.

Either way, the web app's own static host should add its own `Content-Security-Policy`,
`X-Frame-Options` and `Referrer-Policy` (the API sends these on its own responses already; a
static file server generally does not).

## Email

Send from the site's own domain (`no-reply@jobs.example.com`), through a transactional provider.
Before launch:

1. SPF: authorize the provider (`v=spf1 include:<provider> ~all`).
2. DKIM: turn on signing with a 2048-bit key and verify it.
3. DMARC: publish `v=DMARC1; p=none; rua=mailto:dmarc@jobs.example.com`, watch the reports, then
   move to `quarantine`.
4. Send test links to Gmail, Outlook and Yahoo and check they reach the inbox, not spam.
5. Watch bounces and complaints at the provider.

The most links the site will send is bounded by the limits above (`AUTH_MAX_LINKS_PER_HOUR`,
default 300). Set the provider's own daily cap near that.

**CAPTCHA / proof-of-work: not built, by decision.** The per-address, per-IP and site-wide rate
limits (see the design table) are the control for now. Add one (Cloudflare Turnstile is the
first choice: no third-party tracking cookie, works without JavaScript fallback) when either
holds in production for a week: `mail_breaker_open` fires more than once, or `login_rate_limited`
is more than 20% of `login_requested` events over a day. Revisit this decision if either happens.

## Privacy

What the database holds: the email address and the time the account was created and last signed
in; a digest of each unused sign-in link (with the address and times); a digest of each session
(with times, no IP address and no user agent); and the ids of the saved jobs with the time each
was saved. The API logs method, path, status, duration and a request id, never a body, cookie or
token. Sign-in and account events (`login_requested`, `login_verified`, `login_failed`,
`login_rate_limited`, `mail_breaker_open`, `logout`, `logout_all`, `account_deleted`,
`csrf_rejected`, `mail_queue_full`) are logged with the request id and, where an address is
involved, only its masked form: first character, then the top-level domain only (`a***@***.org`,
not the rest of the domain). A mail send failure logs the address's domain only.

Unused and used links are purged 24 hours after they expire; sessions 24 hours after they expire.

### Notice to publish (the operator fills the brackets and has it reviewed)

> **Privacy notice for [Site name], last updated [date]**
>
> [Site name] is run by [operator name], [contact email]. Write to that address for any question
> or request about your data.
>
> **What we keep.** If you sign in: your email address; the ids of the jobs you save and when you
> saved them; a record of your sign-in sessions (when each began and was last used). Our server logs
> record the request and the time, kept for [up to 90 days]. If you do not sign in, we keep nothing
> about you.
>
> **Why.** To let you sign in, save jobs and see them again, and to stop abuse of the sign-in form.
> We send email only to deliver a sign-in link you asked for. We do not send marketing email, sell
> or share your data, show ads or use tracking.
>
> **Who processes it.** [Email provider] delivers sign-in emails. [Hosting provider] hosts the site
> and the database.
>
> **How long.** Sign-in links last 10 minutes. A session ends after 14 days without use, and at the
> latest after 30 days. Your account and saved jobs stay until you delete them. Backups are kept
> [30] days.
>
> **Your rights.** You can delete your account and saved jobs at any time from the account menu; it
> takes effect immediately, and backups drop it within [30] days. You can ask us for a copy of your
> data or a correction at [contact email]. [Reference to the applicable law, for example Ethiopia's
> Personal Data Protection Proclamation No. 1321/2024: confirm with counsel.]
>
> **Cookies.** We set one cookie, `__Host-session`, which keeps you signed in. It is strictly
> necessary, so there is no cookie banner. We set no other cookies.

The web app's `/privacy` page has this text with the operator's details from `VITE_OPERATOR_CONTACT`.

**Cookie decision.** With only the strictly necessary session cookie, and no analytics or
third-party cookies, no consent banner is needed and the notice line above is enough. Adding any
analytics, advertising or third-party cookie reopens this decision.

### Deleting on request

In the app: account menu, Delete account (requires the typed confirmation the UI asks for; the
API in turn requires `{"confirm": true}`, so an empty or accidental request never deletes an
account). For someone who cannot sign in (lost their mailbox), after checking who is asking (the
request should come from the address itself, or answer something only the account holder would
know), run against the production database:

```sql
DELETE FROM users WHERE email = 'person@example.com';           -- sessions and bookmarks cascade
DELETE FROM login_tokens WHERE email = 'person@example.com';
```

**Keep a dated ledger of every manual deletion** (address or a hash of it, date, who asked and
how they were verified, who ran it) — this is what a restore drill (below) replays. Backups keep
the account for as long as their retention window; after restoring from a backup, replay every
deletion in the ledger dated after that backup was taken, before the restored database is used
for anything.

**Restore drill.** At least once, actually restore a backup to a scratch database and confirm:
the schema matches (`aggregator migrate-up` needed, or already applied), a known test account is
present, and replaying the ledger against the restored copy removes exactly the accounts it
should. Date the drill and keep the result with the ledger.

### Secrets: inventory and rotation

| Secret | Where it lives | Rotating it |
|---|---|---|
| `DATABASE_URL` (DB password) | Secret store / environment, never git | Change the password on the DB user, update the stored value, restart `aggregator serve`. The DB role the API connects as should not be a superuser and should have no `DROP`/`CREATE` privilege beyond what its own migrations need. |
| `SMTP_USERNAME` / `SMTP_PASSWORD` | Secret store / environment | Rotate at the mail provider, update the stored value, restart. Sending fails closed (queued mail is retried by the person, not silently dropped) between the old credential being revoked and the new one deployed, so do both within one change window. |
| Session and login tokens | Never stored anywhere as secrets (only their SHA-256 digests are persisted) | Not a rotatable credential; see "Revoking every session" below for the equivalent of "rotate everything at once". |

If a secret is suspected leaked (a credential in a log, a compromised deploy environment):
rotate it immediately by the table above, then check whether the leak could also have exposed
session cookies or login links; if it could have, revoke every session too.

### Revoking every session

After a suspected leak: `DELETE FROM sessions;` (everyone signs in again). This does not cancel
outstanding unused sign-in links; also run `DELETE FROM login_tokens WHERE used_at IS NULL;` if
those might be compromised too.

## Launch checklist

Code is done for the parts above. These need the production environment and must be done before
the site is opened to the public:

- [ ] `PUBLIC_BASE_URL`, `SMTP_*` (including `SMTP_USERNAME`/`SMTP_PASSWORD`), `APP_ENV=production`
      set; `aggregator serve` starts (it refuses an unsafe mix).
- [ ] Migration 000007 applied.
- [ ] HTTPS, redirect and HSTS; the reverse proxy is the one and only layer setting the client-IP
      header, and `CLIENT_IP_HEADER` matches what it sets (see the nginx/Caddy examples above).
- [ ] SPF, DKIM, DMARC set; inbox test to Gmail, Outlook, Yahoo recorded; mail-tester.com score
      recorded.
- [ ] Daily encrypted off-host Postgres backups, retention matching the notice; a restore drill
      done and dated (see "Deleting on request" above); the DB role the API uses is not a
      superuser.
- [ ] SMTP credentials and the database password in the secret store, not in git; the rotation
      table above is filled in and reachable by whoever is on call.
- [ ] A deletion ledger exists (empty is fine) and the manual-deletion process above is written
      down where support staff can find it.
- [ ] Privacy notice filled in (including the processor names and the log/backup retention
      numbers) and published; the operator contact inbox is watched.
- [ ] Point the deploy and the monitor at `/healthz` (liveness) and `/readyz` (database).
- [ ] Alerts on: 5xx rate, 429 rate, send failures, `mail_breaker_open` events, database pool use.
      One test alert delivered end to end.
- [ ] Log retention set to the period in the notice.
- [ ] The cookie-consent decision ("Cookie decision" above: only the strictly necessary
      session cookie, no banner) recorded with a date and an owner's name, not just described
      in this doc — and the note to revisit it if analytics or a third-party cookie is ever
      added carried forward to whoever owns that decision.
- [ ] The kill-all-sessions runbook ("Revoking every session" above) run once, end to end,
      against a non-production environment, and the date and result recorded: sign in, confirm
      `GET /me` works, run `DELETE FROM sessions;`, confirm the same cookie now gets `401`.

## Not built (known gaps)

- Google or other social sign-in. Passwordless email covers sign-in without an external account or
  credentials to manage.
- Per-job link previews (Open Graph) need server-side rendering or a prerender step in front of the SPA.
- Email or Telegram job alerts, and a way to report a wrong eligibility verdict.
- Metrics endpoints and alerting: the API has structured logs only.
- A `Content-Security-Policy` and other security headers on the web app's static host (set them at the
  host or proxy).
- API response compression and `Cache-Control` on the public job endpoints (set at the proxy).
- Suppression of hard-bounced addresses (done at the mail provider).
