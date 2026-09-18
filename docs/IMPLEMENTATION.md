# PodcastDelay — Scope & Implementation Plan

**Status:** design draft, nothing built yet
**Date:** 2026-09-18

## 1. The idea

Take a podcast's real RSS feed and re-publish it as a *new* private RSS feed that
releases episodes from episode #1 forward on a cadence you choose. Point your
podcast app at the new feed and a show that ran weekly for five years starts
arriving weekly, from the beginning, today.

### Decisions already made

| Question | Decision |
| --- | --- |
| Users | **Single user (you).** No accounts, no signup, no multi-tenancy. |
| Timing | **Fixed cadence you choose** (e.g. one episode every 7 days). |
| Audio | **Pass through the publisher's original enclosure URLs.** No proxying, no re-hosting. |
| Stack | **Go** — single static binary + SQLite. |
| Packaging | **Container image**, multi-arch, env-var config, one volume. |

Consequences worth naming up front:

- Pass-through enclosures mean **zero storage and zero egress cost**, and the
  publisher still gets their download counted (most feeds route audio through an
  analytics prefix like `chtbl.com/track/...` or `pdst.fm/e/`, which we keep
  verbatim). It also means if the publisher deletes an episode, it disappears
  for you too. That's an acceptable trade.
- Single-user removes the largest chunk of scope: no auth system, no rate
  limiting per tenant, no abuse handling, no bandwidth budgeting. The whole
  thing is a small CRUD app plus a date calculation.

---

## 2. The central insight

**The delayed feed is computed, not queued.**

There is no job that "publishes an episode every Monday". Given a start date,
a cadence, and a stable ordering of the source episodes, the release time of
episode *n* is pure arithmetic:

```
release_at(n) = start_at + (n - seed_count + 1) * cadence     (n >= seed_count)
release_at(n) = start_at                                       (n <  seed_count)
```

Serving the feed is then one query: *return every episode whose release time is
in the past.* No scheduler, no queue, no drift, no "did the cron run?" failure
mode. The only background work in the whole system is periodically re-fetching
the source feed to notice newly published episodes.

This is the thing that keeps the project small. Everything below is in service
of it.

---

## 3. What makes this non-trivial

The date maths is easy. These are the parts that actually bite:

### 3.1 `pubDate` must be rewritten — this is the whole trick

Podcast apps sort by `pubDate` and use it to decide what counts as new and what
to auto-download. If we emit a 2020 episode with its real 2020 date, your app
files it at the bottom of the list and quite possibly never downloads it.

So every item's `pubDate` becomes its **virtual release date**. The original
date is preserved but moved out of the way — appended to the item description
("Originally published 4 March 2020") and optionally carried in a custom
element. This one transformation is what makes the illusion work.

### 3.2 Ordering must be frozen at ingest

Live feeds are edited constantly: trailers get inserted, episodes get renumbered,
old bonus episodes get backfilled, titles get rewritten. If we re-derive
"episode 37" from the live feed on every request, the schedule shifts under you
and episodes you already received can vanish or repeat.

**Rule: position is assigned once, at first sight, and is immutable.**

- On first ingest, sort all items oldest-first by `pubDate` and assign
  `position = 0..N-1`.
- On every later refresh, any GUID we haven't seen before is **appended** at the
  tail (sorted by `pubDate` among themselves), regardless of how old it claims
  to be. A bonus episode from 2019 that the publisher adds today lands at the
  end of your queue. Slightly unfaithful; far better than reshuffling a
  five-year schedule.
- A GUID that disappears from the source is **tombstoned** (`missing_since`),
  never deleted, and positions never compact. If it was already released, it
  stays in your feed and its audio may 404 — the publisher's doing, not ours.

### 3.3 Already-released episodes must be pinned

If you edit the cadence, pause for a holiday, or exclude an episode, the release
times of *future* episodes should change — and the times of episodes you already
received must **not**. An episode that already appeared in your app with a given
`pubDate` must keep that exact date forever, or the app will re-sort it, re-notify,
or in the worst case re-download it.

**Rule: `scheduled_at` is recomputable while it's in the future, and locked the
moment it passes.** Refresh sets `locked = 1` on any episode whose `scheduled_at`
is now in the past. Rescheduling only ever touches unlocked rows.

The feed query is therefore: `WHERE locked = 1 OR scheduled_at <= now()`.

### 3.4 GUID strategy

Keep each item's original GUID so repeated polls don't look like new episodes.
But **prefix it** (`podcastdelay:{subscription_token}:{original_guid}`) so that
if you also happen to subscribe to the real feed, your app treats them as two
separate shows instead of deduplicating them into one confusing mess.

### 3.5 Seeded episodes need distinct timestamps

If `seed_count = 3`, three episodes share a release moment and therefore a
`pubDate` — apps break ties arbitrarily, so episode 3 might sort above episode 1.
Stagger them: seeded episode *i* gets `start_at + i minutes`. Same for
`episodes_per_release > 1` later on.

### 3.6 DST and wall-clock stability

"Every 7 days at 07:00" should stay 07:00 across a DST boundary. Compute with
`time.Time.AddDate(0, 0, days)` in the subscription's IANA timezone, not by
adding a `168 * time.Hour` duration. Store the timezone per subscription.

### 3.7 Go's `encoding/xml` and namespaced elements

Marshalling `<itunes:duration>` with `encoding/xml` struct tags does not behave
the way you want — Go treats the prefix as a namespace URI and emits surprising
`xmlns` attributes. Two workable routes:

- **Render with `text/template`** and escape every text node explicitly via
  `xml.EscapeText` (or CDATA-wrap descriptions). Full control over prefixes,
  trivially diffable output, easy golden-file tests. **Recommended.**
- Hand-write a marshaller. More code, no real benefit here.

For *parsing*, do not hand-roll. Real feeds are malformed in inventive ways. Use
`github.com/mmcdole/gofeed`, which handles RSS/Atom variants and exposes iTunes
extensions.

### 3.8 Conditional requests, both directions

- **Outbound:** store the source's `ETag` / `Last-Modified` and send
  `If-None-Match` / `If-Modified-Since`. A 304 costs nothing and is polite.
  Set a real `User-Agent` identifying the app.
- **Inbound:** podcast apps poll aggressively. Emit an `ETag` on our feed derived
  from (subscription `updated_at`, highest released position) and answer 304.
  Critically, **do not put a fresh timestamp in `lastBuildDate` on every
  request** — bucket it to the last release event, or apps see perpetual churn.

---

## 4. Data model

SQLite, one file. Embedded migrations.

```sql
CREATE TABLE subscriptions (
  id                  INTEGER PRIMARY KEY,
  token               TEXT NOT NULL UNIQUE,   -- 128-bit random, URL slug
  source_url          TEXT NOT NULL,
  title_override      TEXT,                   -- default: "<Original> (Delayed)"
  cadence_days        INTEGER NOT NULL,       -- 7 = weekly
  release_time        TEXT NOT NULL DEFAULT '07:00',
  timezone            TEXT NOT NULL DEFAULT 'Europe/London',
  start_at            TIMESTAMP NOT NULL,
  seed_count          INTEGER NOT NULL DEFAULT 1,  -- released immediately on day 1
  episodes_per_slot   INTEGER NOT NULL DEFAULT 1,  -- >1 to catch up faster
  shift_seconds       INTEGER NOT NULL DEFAULT 0,  -- accumulated pause time
  paused_at           TIMESTAMP,
  max_feed_items      INTEGER,                -- NULL = unlimited
  channel_json        TEXT NOT NULL,          -- cached channel metadata
  etag                TEXT,
  last_modified       TEXT,
  last_fetched_at     TIMESTAMP,
  last_fetch_status   TEXT,
  created_at          TIMESTAMP NOT NULL,
  updated_at          TIMESTAMP NOT NULL
);

CREATE TABLE episodes (
  id                INTEGER PRIMARY KEY,
  subscription_id   INTEGER NOT NULL REFERENCES subscriptions(id) ON DELETE CASCADE,
  guid              TEXT NOT NULL,
  position          INTEGER NOT NULL,     -- immutable ingest order
  scheduled_at      TIMESTAMP NOT NULL,   -- recomputable only while future
  locked            INTEGER NOT NULL DEFAULT 0,
  excluded          INTEGER NOT NULL DEFAULT 0,
  original_pub_date TIMESTAMP,
  title             TEXT NOT NULL,
  description       TEXT,
  link              TEXT,
  enclosure_url     TEXT NOT NULL,
  enclosure_type    TEXT,
  enclosure_length  INTEGER,
  duration          TEXT,
  episode_number    INTEGER,
  season            INTEGER,
  episode_type      TEXT,                 -- full | trailer | bonus
  explicit          INTEGER,
  image_url         TEXT,
  first_seen_at     TIMESTAMP NOT NULL,
  missing_since     TIMESTAMP,
  UNIQUE (subscription_id, guid),
  UNIQUE (subscription_id, position)
);

CREATE INDEX idx_episodes_feed ON episodes (subscription_id, scheduled_at);
```

`channel_json` caches show-level metadata (title, description, artwork, author,
language, categories, `itunes:type`) so the feed renders without touching the
source. Refreshed on each poll.

---

## 5. HTTP surface

### Public

| Route | Purpose |
| --- | --- |
| `GET /f/{token}.xml` | The delayed feed. Unguessable token is the only access control — podcast apps handle auth badly, so a secret URL is the standard approach (it's what Patreon and Supercast do). |
| `GET /healthz` | Liveness. |

### Admin — behind a single bearer token or HTTP basic auth from env

| Route | Purpose |
| --- | --- |
| `GET /admin` | Server-rendered dashboard: subscriptions, next release, progress ("ep 12 of 240, caught up in 2029"). |
| `POST /admin/subscriptions` | Add a feed. Fetches, parses, ingests, schedules. |
| `PATCH /admin/subscriptions/{id}` | Change cadence / start / seed count. Reschedules unlocked episodes only. |
| `DELETE /admin/subscriptions/{id}` | Remove. |
| `POST /admin/subscriptions/{id}/refresh` | Force a poll. |
| `POST /admin/subscriptions/{id}/pause` `/resume` | Holiday mode; resume adds elapsed time to `shift_seconds`. |
| `GET /admin/subscriptions/{id}/schedule` | Preview the next ~20 release dates. Invaluable for sanity-checking cadence before committing. |

`html/template` server-rendered, no JS build step, no SPA. For a single user this
is a few hundred lines. A small CLI (`podcastdelay add <url> --every 7d
--start tomorrow --seed 2`) is worth having too since it's the fastest path to
adding a show.

---

## 6. Project layout

```
cmd/podcastdelay/main.go      # wiring, flags, graceful shutdown
internal/config/              # env + flags
internal/store/               # SQLite, embedded migrations, queries
internal/source/              # HTTP fetch (conditional GET) + gofeed parse
internal/schedule/            # release-time maths — pure, no I/O, heavily tested
internal/feed/                # RSS rendering (text/template)
internal/web/                 # handlers, admin templates
internal/refresh/             # background poller
testdata/                     # real-world feed samples + golden output
Dockerfile                    # multi-stage, CGO_ENABLED=0, distroless final
compose.yaml                  # reference deployment (see 9.3)
.github/workflows/release.yml # buildx multi-arch image -> ghcr.io
```

`internal/schedule` being pure and I/O-free is deliberate: it's where all the
subtle logic lives (seeding, pausing, locking, rescheduling), and it should be
testable with a fake clock and table-driven cases.

### Dependencies

Deliberately short:

- `github.com/mmcdole/gofeed` — feed parsing.
- `modernc.org/sqlite` — pure-Go SQLite, so the binary stays static with no cgo.
- stdlib for everything else: `net/http`, `text/template`, `encoding/xml`
  (escaping only), `log/slog`.

---

## 7. The refresh loop

A single goroutine, ticking every 30 minutes, picking any subscription whose
`last_fetched_at` is older than its poll interval (default 6h; back off to 24h
after repeated failures). Per subscription:

1. Conditional GET. On 304 → update `last_fetched_at`, done.
2. Parse. On parse failure → record `last_fetch_status`, back off, **change
   nothing**. A broken upstream fetch must never corrupt an existing schedule.
3. Refresh `channel_json`.
4. Upsert episodes: new GUIDs appended at the tail; existing rows get metadata
   updated (titles and descriptions do get corrected upstream) but **never a new
   `position`**; absent GUIDs get `missing_since` set.
5. Lock any episode whose `scheduled_at` has passed.
6. Recompute `scheduled_at` for unlocked, non-excluded episodes.

Everything in one transaction per subscription.

---

## 8. Milestones

Each one ends somewhere you could stop and still have something useful.

**M0 — Walking skeleton (half a day).**
Hardcode one feed URL and a cadence in a config file. Fetch, parse, store,
render a delayed feed at `/f/{token}.xml`. Subscribe to it in your real podcast
app. *This is the step that de-risks the project* — you'll learn immediately
whether your app respects the rewritten `pubDate`, and that's the one assumption
everything rests on. Verify before building anything else.

**M1 — Schedule engine.** `internal/schedule` with seeding, locking,
rescheduling, DST-safe date arithmetic. Full table-driven tests with a fake
clock. Golden-file tests for rendered XML.

**M2 — Persistence + refresh loop.** SQLite, migrations, background poller with
conditional GETs and backoff. Multiple subscriptions.

**M3 — Admin UI + CLI.** Add/edit/pause/delete, schedule preview, progress
display. This is when the project stops needing a redeploy to add a show.

**M4 — Package + deploy.** Multi-arch container image and compose file (§9),
env-var config, embedded migrations run on start, backups, ETag/304 on the
feed, structured logging, and a `/healthz` plus `healthcheck` subcommand.

---

## 9. Deployment and self-hosting

**Packaged as a container.** One image, one volume, one port. That's the whole
operational surface, and it's what makes this runnable on anything from a
Raspberry Pi to a €4 VPS without changing how it's configured.

### 9.1 The image

Multi-stage build: a `golang` builder stage, then a `scratch` or
`gcr.io/distroless/static` final stage. Because the only non-stdlib pieces are
`gofeed` and `modernc.org/sqlite` (pure Go, no cgo), the binary builds with
`CGO_ENABLED=0` and is fully static. Expect roughly 15–20 MB total.

Two things a `scratch` image will otherwise be missing, both of which this app
genuinely needs:

- **CA certificates**, for fetching source feeds over HTTPS. Copy
  `/etc/ssl/certs/ca-certificates.crt` from the builder stage.
- **Timezone data.** The DST-safe scheduling in §3.6 calls `time.LoadLocation`,
  which fails on an empty filesystem and would silently break wall-clock release
  times. The clean fix is a blank import of `time/tzdata` in `main.go`, which
  embeds the IANA database into the binary for about 450 KB. Prefer that over
  mounting the host's zoneinfo — it keeps the image self-contained and makes the
  behaviour identical everywhere.

Build **multi-arch** (`linux/amd64` and `linux/arm64`) with `docker buildx`.
The arm64 target isn't optional if anyone might run this on a Pi or a Synology
or a modern Mac — which, for a home-server app, is most people.

Run as a non-root user with a read-only root filesystem; only the data volume
needs to be writable.

### 9.2 Configuration

Everything through environment variables, so the image needs no rebuild and no
config file baked in:

| Variable | Default | Notes |
| --- | --- | --- |
| `PODCASTDELAY_DATA_DIR` | `/data` | SQLite file lives here. The only writable path. |
| `PODCASTDELAY_ADDR` | `:8080` | Listen address. |
| `PODCASTDELAY_BASE_URL` | — | **Required.** Public URL of the instance. Feeds need an absolute `atom:link rel="self"`, and relative URLs will break in some apps. |
| `PODCASTDELAY_ADMIN_USER` | — | **Required.** |
| `PODCASTDELAY_ADMIN_PASSWORD` | — | **Required.** Refuse to start if unset — never ship a default credential on something that ends up on the open internet. |
| `PODCASTDELAY_DEFAULT_TIMEZONE` | `UTC` | Default for new subscriptions; per-subscription value still wins. |
| `PODCASTDELAY_POLL_INTERVAL` | `6h` | Floor is enforced in code regardless of what's set here. |
| `PODCASTDELAY_LOG_LEVEL` | `info` | |

### 9.3 Compose

```yaml
services:
  podcastdelay:
    image: ghcr.io/samcolson4/podcastdelay:latest
    restart: unless-stopped
    ports:
      - "8080:8080"
    volumes:
      - podcastdelay-data:/data
    environment:
      PODCASTDELAY_BASE_URL: https://podcasts.example.com
      PODCASTDELAY_ADMIN_USER: sam
      PODCASTDELAY_ADMIN_PASSWORD_FILE: /run/secrets/admin_password
      PODCASTDELAY_DEFAULT_TIMEZONE: Europe/London
    secrets:
      - admin_password
    healthcheck:
      test: ["CMD", "/podcastdelay", "healthcheck"]
      interval: 60s

volumes:
  podcastdelay-data:

secrets:
  admin_password:
    file: ./admin_password.txt
```

Supporting a `_FILE` suffix on the password (read the secret from a file rather
than the environment) is a small convention that self-hosters expect and costs
about ten lines. The healthcheck invokes the binary itself rather than curl,
since a distroless image has no shell or HTTP client.

### 9.4 Reachability is the real constraint, not compute

The resource footprint is trivial — parsing some XML every six hours and
serving a few kilobytes on request, idling around 20–30 MB of RAM. Nothing here
strains a Pi. The actual friction is that **your podcast app needs to reach the
feed from a phone on mobile data.** Options, roughly in order of effort:

- **Cloudflare Tunnel** — no port forwarding, no dynamic DNS, TLS handled, free.
  The lowest-effort path for a box behind a home router, and the one to
  recommend in the README.
- **Tailscale** — clean and private, but your phone has to be on the tailnet for
  the podcast app's background refresh to succeed. Fine if you already run it;
  a bit fragile as the only access path.
- **Dynamic DNS + a reverse proxy** (Caddy for automatic certificates) — the
  traditional route, worth it if you already have that furniture.
- **Skip the house entirely** and run the same image on a small VPS or Fly.io.

Serve over TLS regardless of route. iOS App Transport Security makes plain HTTP
enclosure and feed URLs an unnecessary fight.

### 9.5 Downtime tolerance is higher than it looks

This matters because it's the main argument against hosting at home, and the
argument is weaker than it appears. (An earlier draft of this doc claimed a
feed that 404s for a week is "how you silently lose episodes" — that was
wrong.)

Release times are computed and locked server-side (§2, §3.3), so an outage
**does not consume your schedule**. When the box comes back, every episode that
released during the gap is sitting in the feed waiting. You lose timeliness,
not episodes.

The residual risk is milder: some apps only auto-download the newest few items,
so after a long outage you may have to tap download on a backlog manually. Worth
knowing, not worth architecting against.

### 9.6 Backups

The SQLite file is the entire state — subscriptions, positions, and which
episodes have already been released. It is small and it is irreplaceable:
restoring from nothing means every show restarts from episode 1.

- `litestream` for continuous replication to object storage, or
- a daily `sqlite3 .backup` to the same, or
- a volume snapshot if the host provides one.

Any of the three is fine. Having none is the actual mistake.

### 9.7 If it's packaged for other people

The single-user decision (§1) is not an obstacle to distribution — self-hosters
run their own instance, so one user per deployment is the native model for that
ecosystem. Beyond what's already above, shipping it to others needs:

- Versioned image tags (`:1.2.3`, `:1`, `:latest`), not just `latest`
- An example `compose.yaml` and a README covering reverse proxy and backups
- Embedded migrations that run automatically on start, so upgrades are just
  pulling a new tag
- No hardcoded paths, no assumption of a writable working directory
- A `healthcheck` subcommand, which Unraid/CasaOS/TrueNAS-style app catalogues
  and orchestrators both want

On the legal side, distributing the software is a materially different
proposition from running a hosted service: each operator chooses the feeds
themselves, and the pass-through design (§1) means nobody is redistributing
audio. That's a far more comfortable position than multi-tenant hosting, and
another reason to stay out of that business.

Cost, whichever way it's hosted: XML only, a few kilobytes per poll.
Effectively free.

---

## 10. Edge cases to handle explicitly

| Case | Handling |
| --- | --- |
| Item with no `pubDate` | Fall back to feed order, then `first_seen_at`. Never drop it. |
| Duplicate GUIDs in source | First wins; log the rest. More common than you'd hope. |
| Item with no enclosure | Skip (it's usually a text-only post), log it. |
| Trailers / "start here" items | Optional `skip_episode_types` to filter `itunes:episodeType` = trailer. |
| Feed already caught up | Once the tail is reached, new episodes release at your cadence — meaning if the show goes weekly and your cadence is weekly, you just track it live. Nothing special needed. |
| Cadence faster than the original | Perfectly fine, and how you catch up. `episodes_per_slot` for aggressive catch-up. |
| Excluded episode | Leaves a gap in the schedule rather than compacting it, so already-released dates never move. Simpler and safer; a "compact unreleased" option can come later. |
| Very long feeds (1000+ items) | `max_feed_items` caps the rendered window. Not urgent — the feed grows one item a week — but cheap insurance against apps that choke. |
| Source redirects (301 to a new host) | Follow, and persist the new URL on a permanent redirect. Feed migrations are routine. |
| Source dies entirely | Keep serving from the DB. Already-ingested episodes keep releasing; only *new* ones stop. A nice property of ingesting rather than proxying. |

---

## 11. Ethics and legality, briefly

Worth being straight about since this republishes someone else's feed:

- It's for **your own private listening**, on an unlisted URL. Not a public
  mirror, not a directory.
- Audio is **not re-hosted and not modified** — your app fetches from the
  publisher's CDN through their own analytics prefix, so their download is
  counted and their ads are intact. In download-stats terms you look like an
  ordinary listener, which is the honest outcome.
- Don't strip sponsorships, don't remove attribution, keep the artwork and
  credits.
- Be polite on fetches: conditional GETs, a 6h poll floor, an identifying
  User-Agent.

If this ever became a public service, the calculus changes (redistribution at
scale, publishers' feed terms, App Store directories) — which is a good reason
to keep it single-user.

---

## 12. Deliberately out of scope

Named so they don't creep in:

- Multi-user accounts, billing, a public web app.
- Hosting or transcoding audio.
- "Mirror the original release gaps" time-shifting. A real alternative to fixed
  cadence and a natural v2 — the schema supports it (swap the `schedule`
  implementation) — but not now.
- Resolving Apple/Spotify show links to RSS URLs. Convenient, one iTunes Lookup
  API call, but not v1.
- OPML import/export.
- Listen-tracking, playback position, anything client-side.

---

## 13. Open questions for later

1. **Does your podcast app honour rewritten `pubDate`s the way we need?** M0
   answers this. Overcast, Pocket Casts and Apple Podcasts all behave well in
   principle; verify with yours before building the rest.
2. Should reaching the live tail switch the feed to real-time automatically, or
   keep pacing at your cadence forever? (Default: keep pacing — simpler, and
   arguably the point.)
3. Do you want a "bump" control — skip this week, or release the next one now —
   for when you're ahead or behind? Cheap to add on top of `shift_seconds`.
