# stalker: design spec

A personal, self-hosted, Fraidycat-style follow tracker. Single Go binary, SQLite, server-rendered HTML with htmx and SSE.
Runs on a VPS behind a TLS reverse proxy, single user, HTTP basic auth.

## Layout (module `github.com/oryanm/stalker`, Go 1.26)

```
cmd/stalker/        main: subcommands serve | import | export | check | discover
internal/model/     shared types (DONE, do not change without updating every user)
internal/store/     SQLite (modernc.org/sqlite, pure Go, no cgo), embedded migrations
internal/feed/      HTTP client with per-host throttling, gofeed-based parsing
internal/feedurl/   stored feed URL -> fetched address (YouTube channel pages, old.reddit), the feed identity
internal/discover/  URL -> verified feed candidates
internal/opml/      Fraidycat-compatible OPML import/export
internal/events/    in-process pub/sub for SSE
internal/poller/    importance-driven scheduler
internal/web/       handlers, templates (embed), static (embed; htmx 2.0.11 + sse ext already vendored)
testdata/fraidycat-sample.opml   a synthetic export in Fraidycat's exact format (14 follows, emoji tags, all tiers)
```

The exported signatures in each package's `*.go` stub are the contract between packages. Implement them exactly. You may add
unexported helpers, extra files and extra exported helpers, but do not change or remove contract signatures. If a contract is
genuinely wrong, make the smallest change, update every caller, and report it.

## Conventions

- Go 1.26 idioms: `net/http` ServeMux patterns (`GET /follows/{id}`), `log/slog`, `math/rand/v2`, `for range n`, `errors.Is`,
  `testing/synctest` where it helps. `gofmt`-clean, `go vet`-clean.
- Dependencies already in go.mod: `github.com/mmcdole/gofeed`, `modernc.org/sqlite`, `github.com/PuerkitoBio/goquery`. Do not add
  others without a strong reason. Never run `go mod tidy` while other packages are still stubs (it would drop deps); only the
  integration step runs it.
- Comments: sparse, explain why not how, one `//` line where possible.
- Every package has table-driven `_test.go` tests using only the standard library (`httptest`, `t.TempDir()`). No network in unit
  tests. Live network is only used by the integration step.
- Times are stored as UTC. SQLite columns hold times as INTEGER unix milliseconds (0 = zero time) to keep sorting and comparisons
  trivial.

## Fraidycat behaviour to reproduce

- Follows have tags and one importance tier (see `model.Tiers`). Follows without tags live under the 🏠 tag
  (`model.HomeTag`); follows tagged `🏠` explicitly also show there. Tag tabs: 🏠 first, then the other tags sorted.
- Within a tag, tier sub-tabs are shown for tiers that have follows, and Realtime is always shown. The default tier is the most
  important tier present in that tag.
- A follow row shows: title (links to the site), how long ago its latest post was (`timeAgo` below), an activity sparkline, the
  titles of its latest posts (each linked, each with its own age), an error marker when the last fetch failed, and edit/refresh
  controls. Rows are sorted by latest post, newest first. Sort options are "Recent posts" (default), "Recently followed" and
  "A to Z", stored as a setting.
- Freshness colour classes (on the row and on each post): `age-h` if at most 3 days old, `age-d` if at most 30 days, else `age-M`.
  Following Fraidycat, age-h is dark green, age-d is cyan/teal, age-M is a muted light brown. A tag tab gets the colour of the
  newest post among that tag's Realtime follows.
- `timeAgo(t, now)`: under 1 minute `1m`, 1-45 minutes `Nm`, 46-90 minutes `1h`, up to 24h `Nh`, up to 48h `1d`, up to 72h `2d`,
  up to a year `Jan 2`, older `Jan 2, 2006`. Zero time renders nothing.
- Sparkline: a 120x20 inline SVG polyline of posts per day, newest on the right. Use the last 60 days (pink stroke) if there was any
  activity in them, else the last 180 days bucketed per 3 days (grey stroke). Render nothing when there is no activity.
- Polling: see `poller.Interval` / `poller.NextFetch`. Conditional GETs (ETag / Last-Modified). Per-host throttling in
  `feed.Client` keeps YouTube (84 feeds) from rate limiting us.
- OPML: Fraidycat exports `category="importance/7,👨‍💻"`, `created="Wed Jun 24 2026 10:28:09 GMT-0400 (Eastern Daylight Time)"`,
  `text`, optional `title`, `xmlUrl`, `htmlUrl`. Export writes the same shape so the file can go back into Fraidycat.

## Web UI

Server-rendered `html/template`; `hx-boost="true"` on `<body>` so every link and form also works without JS; htmx fragments for
expanding a follow's posts, the add/edit forms and refresh; the htmx SSE extension (`sse-connect="/events"`) swaps a follow's row
summary in place when `events.FollowUpdated` fires (the server renders the fragment and sends it as SSE event `follow-<id>`).
Rows do not reorder live (so open forms and expanded rows are never clobbered); a reload reorders.

Routes (all behind basic auth except `/healthz` and `/static/`):

| Route | Purpose |
|---|---|
| `GET /` | follows list, query `tag`, `tier` |
| `GET /follows/{id}/posts` | fragment: up to 20 recent posts, plus a collapse control |
| `GET /add` | add form, query `url`, `tag`, `tier` prefill |
| `POST /follows` | discover; 1 candidate: create, `FetchNow` (bounded wait ~20s), redirect to the follow's tag/tier; several: render a chooser (checkboxes) that posts back to `POST /follows/choose`; none: re-render form with the error |
| `POST /follows/choose` | create the selected candidates |
| `GET /follows/{id}/edit` | edit form: title, site URL, feed URL, tags (space- or comma-separated), tier, delete button |
| `POST /follows/{id}` | save edit |
| `POST /follows/{id}/delete` | delete, redirect |
| `POST /follows/{id}/refresh` | `FetchNow`, return the row summary fragment |
| `GET /settings` | import form, export link, sort setting, counts, list of follows currently in error |
| `POST /settings` | save sort setting |
| `POST /import` | multipart OPML upload: parse, `ImportFollows`, `poller.Kick()`, show added/skipped counts |
| `GET /export.opml` | download OPML |
| `GET /events` | SSE stream, `: ping` comment every 25s, flushes after every event |
| `GET /healthz` | `ok` |

Security (it runs on a VPS): basic auth with constant-time compare; `http.CrossOriginProtection` (Go 1.25+) on the mux; CSP
`default-src 'self'; img-src 'self' https: data:; style-src 'self'; script-src 'self'; frame-ancestors 'none'` (so no inline
scripts, styles or event handlers); `X-Content-Type-Options: nosniff`; `Referrer-Policy: no-referrer`. Post titles and all feed
data are text only, never raw HTML. External links get `rel="noopener noreferrer"` and open in a new tab. Upload size capped
(5 MiB).

Look: clean and readable, Fraidycat-inspired (serif-ish headings, generous line height, coloured ages), light and dark via
`prefers-color-scheme`. One CSS file in `static/`. No external fonts or CDNs.

## CLI

```
stalker serve    [-addr 127.0.0.1:8080] [-db data/stalker.db]
stalker import   [-db ...] file.opml          # prints added/skipped
stalker export   [-db ...] > file.opml
stalker check    [-db ...] [-concurrency 8]   # fetch every follow now, print a table, exit 1 if any failed
stalker discover URL                          # print candidates
```

Env overrides: `STALKER_ADDR`, `STALKER_DB`, `STALKER_USERNAME` (default `stalker`), `STALKER_PASSWORD`, `STALKER_NO_AUTH=1`,
`STALKER_ALLOWED_HOSTS` (Host names served without auth besides localhost and IP addresses), `STALKER_ALLOW_PRIVATE=1`
(outbound requests may reach loopback and private networks), `TZ`.
`serve` refuses to start without a password unless `STALKER_NO_AUTH=1`. Graceful shutdown on SIGINT/SIGTERM (stop poller, drain
HTTP with a 10s timeout). Deployment files: multi-stage `Dockerfile` (CGO_ENABLED=0, static binary, non-root, `/data` volume),
`docker-compose.yml`, `README.md` (including a Caddy reverse proxy snippet).
