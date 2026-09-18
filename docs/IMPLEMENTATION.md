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

**M4 — Deploy + polish.** Container image, backups, ETag/304 on the feed,
structured logging, a `/healthz` you can point an uptime checker at.

---

## 9. Deployment

A single static binary and a SQLite file. Options, cheapest first:

- **Fly.io** — one shared-cpu-1x machine with a small persistent volume, TLS and
  a hostname included. Note that a scale-to-zero machine still wakes on the
  first poll from your podcast app, so cold starts are harmless here.
- **Any €4/mo VPS** (Hetzner, etc.) with Caddy in front for automatic TLS.
- **A Raspberry Pi at home** would work too, but your podcast app needs the feed
  reachable from outside the house, and a feed that 404s for a week is how you
  silently lose episodes. Prefer something hosted.

Backups: the SQLite file is the entire state and it is tiny. `sqlite3 .backup`
on a daily cron to object storage, or just `litestream`. Worth doing — losing it
means losing your position in every show.

Cost: XML only, a handful of kilobytes per poll. Effectively free.

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
