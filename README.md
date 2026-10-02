# PodcastDelay
## What
A solution for re-publishing a podcast's real RSS feed as a new private feed. It can start releasing from any episode, at any cadence, including mirroring the original release cadence.

This is designed only to be run in a homelab or similar. It is single-user, with no support for separate 'accounts'. Audio is never re-hosted — enclosure URLs (and the publisher's analytics prefix) pass through untouched, so your listens still count. See [docs/IMPLEMENTATION.md](docs/IMPLEMENTATION.md) for the full (fully-Claude) design write-up.

## Why
I'm a bit nostalgic. There are some podcasts I started listening to years ago (2017~), but not at the start (2008~). At a certain point, the podcast changes, maybe some stalwarts rotate out of the main line-up... I just want to go back, to the beginning.

I'm also someone that has no self-restraint. I'll mainline ten of the same podcast in a day, then forget all about listening to no.11. Podcasts apps do not help this: It's hard to get them to cooperate in a way which surfaces _your_ next episode in the 'Latest episodes' list, because while it may be up next for _you_, it was actually released 15 years ago.

PodcastDelay aims to solve that, by creating personalised feeds.

## Quick start (Docker Compose)
```bash
cp .env.example .env   # then set PODCASTDELAY_BASE_URL and PODCASTDELAY_ADMIN_USER
echo "a-strong-password" > admin_password.txt
docker compose up -d
```

Then open `http://localhost:8080/admin` (basic auth: the user/password you configured) and add a feed.

Required environment variables — the container refuses to start without them:

| Variable | Notes |
| --- | --- |
| `PODCASTDELAY_BASE_URL` | Public URL of this instance, e.g. `https://podcasts.example.com`. |
| `PODCASTDELAY_ADMIN_USER` | Admin basic-auth username. |
| `PODCASTDELAY_ADMIN_PASSWORD` (or `_FILE`) | Admin basic-auth password, or a path to a file containing it. |

See `docs/IMPLEMENTATION.md` §9.2 for the full list of variables and defaults.

## Building and running locally

```bash
go build ./cmd/podcastdelay
PODCASTDELAY_DATA_DIR=./data \
PODCASTDELAY_BASE_URL=http://localhost:8080 \
PODCASTDELAY_ADMIN_USER=admin \
PODCASTDELAY_ADMIN_PASSWORD=changeme \
./podcastdelay serve
```

Add a feed without opening the admin UI:

```bash
./podcastdelay add https://feeds.example.com/show.xml \
  --every 7d --start tomorrow --seed 2
```

## Reachability
Your podcast app needs to reach the feed from a phone on mobile data.
[Cloudflare Tunnel](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/) is the lowest-effort  path for a box behind a home router (no port forwarding, free TLS). See docs §9.4 for alternatives.

## Backups
The SQLite file at `$PODCASTDELAY_DATA_DIR/podcastdelay.db` is the entire state. Back it up with [litestream](https://litestream.io), a daily `sqlite3 .backup`, or a volume snapshot — see docs §9.6.

## Development
```bash
go test ./...              # unit + golden-file tests
UPDATE_GOLDEN=1 go test ./internal/feed/... -run TestRender_Golden
docker buildx build --platform linux/amd64,linux/arm64 -t podcastdelay .
```

`internal/schedule` is the pure release-time engine (no I/O) — start there if
you're trying to understand the seeding/locking/rescheduling rules.
