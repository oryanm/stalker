# stalker

A personal, self-hosted follow tracker in the spirit of [Fraidycat](https://fraidyc.at). You follow blogs, YouTube channels,
subreddits and anything else with an RSS, Atom or JSON feed, and stalker shows each follow as one row: when it last posted, a
sparkline of its activity and the titles of its latest posts. Follows are grouped by tag and by how closely you watch them
(importance tiers), so a quiet friend's blog is not buried under a busy news site.

It is a single Go binary with an embedded SQLite database and a server-rendered UI (htmx, live updates over SSE). It is built
for one user on a small VPS behind a TLS reverse proxy, with HTTP basic auth. OPML import and export use Fraidycat's format, so
you can move your follows in both directions.

## Quick start

With Go 1.26 or later:

```sh
go install github.com/oryanm/stalker/cmd/stalker@latest

# import your Fraidycat export (an OPML file from Fraidycat's settings page)
stalker import fraidycat.opml

# serve on http://127.0.0.1:8080 without a password (local use only)
STALKER_NO_AUTH=1 stalker serve
```

From a clone, use `go run ./cmd/stalker` instead of `stalker`. `testdata/fraidycat-sample.opml` is a small Fraidycat-format
export to try it with.

The database is created at `data/stalker.db`. Every imported follow is due at once, so the first round of fetches starts
immediately and rows fill in as feeds arrive. You can also import from the Settings page, or add follows by pasting any page
URL on the Add page: stalker discovers the feed for you.

## Deploying on a VPS

You need Docker with the Compose plugin and a DNS name pointing at the server.

```sh
git clone https://github.com/oryanm/stalker.git && cd stalker
echo "STALKER_PASSWORD=$(openssl rand -base64 24)" > .env    # note it down: it is your login
docker compose up -d --build
```

Post dates are shown in the `TZ` timezone, which `docker-compose.yml` sets to `America/Toronto`; add `TZ=Europe/Paris` (or
your zone) to `.env` to change it.

The container listens on `127.0.0.1:8080` only; put a reverse proxy in front for TLS. With
[Caddy](https://caddyserver.com) installed on the host, this `/etc/caddy/Caddyfile` is all it takes (Caddy obtains and renews
the certificate itself):

```caddyfile
stalker.example.com {
	reverse_proxy 127.0.0.1:8080
}
```

Then `sudo systemctl reload caddy` and open `https://stalker.example.com`, logging in as `stalker` with the password from
`.env`. Caddy streams `text/event-stream` responses unbuffered, so live updates work without extra settings (avoid adding
`encode`, which can hold them back). With another proxy, turn off response buffering for `/events` (nginx:
`proxy_buffering off;`).

To run Caddy in Compose instead, add it next to the `stalker` service, drop the `ports:` mapping from `stalker`, and use
`reverse_proxy stalker:8080` in the Caddyfile:

```yaml
  caddy:
    image: caddy:2
    restart: unless-stopped
    ports: ["80:80", "443:443", "443:443/udp"]
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile:ro
      - caddy-data:/data
```

(and `caddy-data:` under the top-level `volumes:`).

The image is distroless (no shell), runs as a non-root user and keeps everything in the `stalker-data` volume. It has no
Docker `HEALTHCHECK` because there is no shell or curl inside; point an uptime monitor at `https://stalker.example.com/healthz`,
which answers `ok` without authentication. To update: `git pull && docker compose up -d --build`. Stopping the container is
safe at any time: stalker stops polling, records the fetches already answered and drains HTTP connections within 10 seconds.

CLI commands run inside the container, which already has `STALKER_DB` set:

```sh
docker compose exec -T stalker /stalker import - < fraidycat.opml
docker compose exec -T stalker /stalker export > follows.opml
docker compose exec stalker /stalker check
```

## Configuration

Environment variables; the matching flags take precedence.

| Variable                | Default                                 | Meaning                                                                                        |
|-------------------------|-----------------------------------------|------------------------------------------------------------------------------------------------|
| `STALKER_ADDR`          | `127.0.0.1:8080` (`:8080` in the image) | listen address for `serve` (`-addr`)                                                           |
| `STALKER_DB`            | `data/stalker.db` (`/data/stalker.db`)  | SQLite database path, created on first use (`-db`)                                             |
| `STALKER_USERNAME`      | `stalker`                               | basic auth user                                                                                |
| `STALKER_PASSWORD`      |                                         | basic auth password; `serve` refuses to start without it                                       |
| `STALKER_NO_AUTH`       |                                         | `1` disables authentication; only for a trusted machine                                        |
| `STALKER_ALLOWED_HOSTS` |                                         | with `STALKER_NO_AUTH`, comma-separated host names served besides `localhost` and IP addresses |
| `STALKER_ALLOW_PRIVATE` |                                         | `1` lets fetches and discovery reach loopback and private networks                             |
| `STALKER_LOG_LEVEL`     | `info` for `serve`, `error` otherwise   | `debug` (logs every fetch and request), `info`, `warn`, `error`                                |
| `TZ`                    | the system's (`UTC` in the image)       | timezone of the dates shown on posts, such as `America/Toronto`                                |

Logs go to stderr as `key=value` lines.

Feeds, their redirects and the URLs you paste are fetched from the server, so by default stalker refuses to connect to
loopback, private (`10/8`, `172.16/12`, `192.168/16`, `fc00::/7`), link-local (including cloud metadata at
`169.254.169.254`) and carrier-grade NAT addresses. The check runs after DNS resolution on every connection. Set
`STALKER_ALLOW_PRIVATE=1` to follow feeds on your own network, or when an `HTTP_PROXY` on such an address must be used.

Without authentication, `serve` only answers requests whose `Host` is `localhost`, an IP address or one of
`STALKER_ALLOWED_HOSTS`, so a web page cannot reach it through DNS rebinding.

## CLI

```
stalker serve    [-addr 127.0.0.1:8080] [-db data/stalker.db]
stalker import   [-db path] file.opml          # prints "added N, skipped M"; "-" reads stdin
stalker export   [-db path] > file.opml        # Fraidycat-compatible OPML
stalker check    [-db path] [-concurrency 8]   # fetch every follow now, print a table, exit 1 if any failed
stalker discover URL                           # list the feeds found at URL
stalker help [command]
```

- `serve` runs the web UI and the background poller until SIGINT or SIGTERM.
- `import` skips feed URLs you already follow, so importing the same file twice is harmless. Folders become tags; the
  `importance/N` category sets the tier.
- `export` writes `category="importance/N,tag,..."`, `created`, `text`, `title` (only when you renamed the follow), `xmlUrl`
  and `htmlUrl`, which Fraidycat imports as is.
- `check` fetches every follow once, records the results like the poller does and prints status, title, post count, time
  taken and error for each one. It is handy after an import, or from cron to spot dead feeds.
- `discover` prints what the Add page would offer for a URL: YouTube channels, handles and playlists, subreddits and Reddit
  users, GitHub users and repositories, Bluesky profiles, Medium, Substack, `<link rel="alternate">` feeds and common feed
  paths.

`export`, `check` and `import` can run while `serve` is running on the same database.

## Themes

Settings has a theme picker. **Receiver** (the default) is a 1970s stereo receiver in the
[Darcula Forest](https://github.com/oryanm/darcula-forest) colours, with a tuning dial that places each follow at the age of
its latest post. **Classic** is the original Fraidycat-style list and follows your system's light or dark setting.

A theme is a single stylesheet over the same HTML: add `internal/web/static/themes/<id>.css` and an entry in the `themes` list
in `internal/web/theme.go`.

## Importance tiers and polling

| Tier       |    | OPML category    | Checked every |
|------------|----|------------------|---------------|
| Realtime   | 🚄 | `importance/0`   | 10 to 15 min  |
| Frequent   | 🌄 | `importance/1`   | 1 to 1.5 h    |
| Occasional | 🐇 | `importance/7`   | 4 to 6 h      |
| Sometime   | 🍊 | `importance/30`  | 12 to 18 h    |
| Rarely     | ☂  | `importance/365` | 24 to 36 h    |

- The interval is the tier's base plus up to 50% random jitter, so follows imported together drift apart.
- **Update limit** (Settings): a minimum interval such as `3h`, `90m` or `2d`. Tiers checked more often than that slow down
  to it (with a 3h limit, Realtime and Frequent are checked every 3 hours) and slower tiers are unaffected. Changing it
  reschedules follows right away. The refresh button and newly added follows ignore it.
- Requests are conditional (`If-None-Match`, `If-Modified-Since`); an unchanged feed costs one `304`.
- After consecutive failures the delay doubles from the base (up to 64 times it), capped at 24 hours plus jitter, and never
  sooner than a `Retry-After` the server sent. One success resets it.
- A follow's row only shows a warning once it has been failing for 24 hours (straight away if it has never had a post),
  so a feed that is down for a few hours stays quiet. YouTube's feeds sometimes return false 404s for hours at a time.
  Settings lists every failing follow, including the ones not flagged yet.
- Up to 6 feeds are fetched at once, and at most 2 per host with 500 ms between requests, which keeps dozens of YouTube feeds
  from tripping rate limits. Each request times out after 30 seconds.
- The refresh button on a row fetches it immediately, whatever its schedule.
- Each follow keeps its newest 50 posts plus anything from the last 180 days, up to 500 posts, and whatever its feed still
  lists. Posts the feed drops although they are newer than its oldest entry (deleted or re-uploaded videos) are removed, as
  Fraidycat does. At most 200 entries are read from one fetch.

## Backups

Everything lives in one SQLite file (WAL mode, so `stalker.db-wal` and `stalker.db-shm` sit next to it while it runs).

A consistent copy while stalker keeps running, using SQLite's online backup:

```sh
docker run --rm -v stalker-data:/data:ro -v "$PWD":/backup alpine \
  sh -c 'apk add -q sqlite && sqlite3 /data/stalker.db ".backup /backup/stalker-$(date +%F).db"'
```

Outside Docker, `sqlite3 data/stalker.db ".backup stalker-backup.db"` does the same. Alternatively stop the container
(`docker compose stop`), copy the files out of the volume and start it again; a clean stop leaves everything in
`stalker.db`. The OPML export is a lighter backup of just the follows, without posts or fetch state.

To restore, stop the container, put the backup in the volume as `stalker.db` (deleting any `-wal` and `-shm` files), make it
owned by UID 65532 and start the container.

## Development

```sh
go test -race ./...
go vet ./...
```

Tests use only the standard library, temporary databases and in-process HTTP servers: no network. The layout is described in
[SPEC.md](SPEC.md): `internal/store` (SQLite), `internal/feed` (throttled fetching and parsing), `internal/discover`,
`internal/opml`, `internal/poller`, `internal/events` and `internal/web`, with the binary in `cmd/stalker`.
