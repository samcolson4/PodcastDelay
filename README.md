# PodcastDelay

Re-publish a podcast's real RSS feed as a new private feed that releases
episodes from #1 forward on a cadence you choose. Point your podcast app at
the new feed and a show that's been running for years starts arriving on a
schedule, from the beginning, today.

Single user, no accounts. Audio is never re-hosted — enclosure URLs (and the
publisher's analytics prefix) pass through untouched, so your listens still
count. See [docs/IMPLEMENTATION.md](docs/IMPLEMENTATION.md) for the full
design write-up.

## Quick start (Docker Compose)

```bash
# edit compose.yaml: set PODCASTDELAY_BASE_URL and PODCASTDELAY_ADMIN_USER
echo "a-strong-password" > admin_password.txt
docker compose up -d
```

Then open `http://localhost:8080/admin` (basic auth: the user/password you
configured) and add a feed.

Required environment variables — the container refuses to start without
them:

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
[Cloudflare Tunnel](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/)
is the lowest-effort path for a box behind a home router (no port forwarding,
free TLS). See docs §9.4 for alternatives.

## Backups

The SQLite file at `$PODCASTDELAY_DATA_DIR/podcastdelay.db` is the entire
state. Back it up with [litestream](https://litestream.io), a daily
`sqlite3 .backup`, or a volume snapshot — see docs §9.6.

## Development

```bash
go test ./...              # unit + golden-file tests
UPDATE_GOLDEN=1 go test ./internal/feed/... -run TestRender_Golden
docker buildx build --platform linux/amd64,linux/arm64 -t podcastdelay .
```

`internal/schedule` is the pure release-time engine (no I/O) — start there if
you're trying to understand the seeding/locking/rescheduling rules.
