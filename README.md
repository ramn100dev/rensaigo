<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/img/logo-dark.svg">
    <img src="docs/img/logo-light.svg" alt="Rensaigo" height="72">
  </picture>
</p>

**[Español](README.es.md)**

*連載号, "serial issue": the next chapter of every series you follow, on its own.*

Self-hosted manga manager in a single container. Follow a series and new chapters download on their own, in a
"drop and forget" fashion, from **Mihon/Tachiyomi extensions**. Your library stays as plain CBZ files with
`ComicInfo.xml`, and you read it in the built-in reader or in any OPDS app.

It is small: an always-on **Go core** (UI, API, SQLite, scheduler, download queue, reader) and a thin **JVM host**
that runs the extensions **only while there is work** and then shuts down. Idle it uses a few tens of MB of RAM.
Cloudflare-protected sources go through an external **FlareSolverr**; the image ships no browser.

## Features

- **Library**: covers, filters (tracking, reading state, genre, source), favourites, progress per series.
- **Several sources per series**: a main source plus backups, in priority order. Chapters missing from one are taken
  from another, and a chapter can be replaced later by a better copy (higher resolution) from a higher-priority source.
- **Built-in reader**: vertical (webtoon) or paged, right-to-left manga mode, remembers where you stopped.
- **Downloads**: queue shared fairly between series, retries with backoff, global pause, page verification.
- **Extensions**: install from the repository with signature check, automatic updates, keep the 2 previous
  versions and roll back or pin one, per-extension preferences, "+18" versions folded into the normal one.
- **Source coverage**: measures every extension of a language against *your* library (how many of your series it
  has, how many chapters, page resolution) and ranks them, so you know which ones are worth installing.
- **Tracking**: MyAnimeList, AniList and MangaDex (read chapters and status are pushed automatically).
- **Notifications**: Telegram, ntfy, Discord, Gotify or a webhook when new chapters arrive or something fails.
- **OPDS** feed for external readers (Panels, Chunky, KOReader, Mihon…).
- **Import**: adopts an existing CBZ library, including the `rensaio.json` files of **Rensaiō / Kaizoku**,
  without downloading anything again.
- **Mobile friendly** UI (server-rendered, htmx, no Node).
- Secrets (tokens, notification channels) are stored encrypted; compact database backups on a schedule.

## Quick start

```sh
curl -O https://raw.githubusercontent.com/ramn100dev/rensaigo/main/docker-compose.yml
mkdir -p library
docker compose up -d
```

Open `http://<server>:9840`. Then:

1. **Sources** → install the extensions you want (the index of the default repository is already loaded).
2. **Library** → **Add** → search a title in your sources and follow it.
3. Already have a library? **Settings → Import** adopts the CBZ files under `/library/<Category>/<Series>/`.

The library folder must be writable by the container user (`1000:1000` by default).

### Docker run

```sh
docker run -d --name rensaigo --restart unless-stopped \
  -p 9840:9840 \
  -e RENSAIGO_FLARESOLVERR_URL=http://flaresolverr:8191 \
  -v rensaigo-config:/config \
  -v /path/to/manga:/library \
  --user 1000:1000 \
  ghcr.io/ramn100dev/rensaigo:latest
```

## Configuration

Almost everything is set from **Settings** in the UI (download folder, check interval, concurrency, backups…).
Environment variables are for what must be known before starting:

| Variable | Default | |
|---|---|---|
| `RENSAIGO_ADDR` | `:9840` | listen address |
| `RENSAIGO_PASSWORD` | (empty) | single password; **empty = no login**, don't expose it like that |
| `RENSAIGO_FLARESOLVERR_URL` | (empty) | FlareSolverr for Cloudflare-protected sources |
| `RENSAIGO_REPO_URL` / `RENSAIGO_REPO_FINGERPRINT` | Keiyoushi | extension repository and the SHA-256 of the certificate that must sign its jars |
| `TZ` | UTC | time zone |

Full list: [docs/configuration.md](docs/configuration.md).

## Building

```sh
docker build -t rensaigo -f docker/Dockerfile .
```

The first build takes a few minutes (Gradle, dependencies and the pinned Suwayomi code the host is built on).
Development notes: [docs/development.md](docs/development.md).

## License

[MIT](LICENSE). Bundled: [htmx](https://htmx.org) (0BSD) and the Plus Jakarta Sans font ([OFL](web/static/fonts/OFL.txt)).

## Credits

- Extensions run on **AndroidCompat** from [Suwayomi-Server](https://github.com/Suwayomi/Suwayomi-Server) (MPL-2.0),
  downloaded and pinned at build time.
- Extensions come from the [Keiyoushi](https://github.com/keiyoushi/extensions) repository; none are bundled.
- Inspired by [Rensaiō](https://github.com/maxpiva/Rensaio) and Kaizoku. This is an independent project, not a fork.
