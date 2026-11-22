# Configuration

Settings marked with ⚙ can also be changed from **Settings** in the UI; the value saved there wins over the variable.

| Variable | Default | |
|---|---|---|
| `RENSAIGO_ADDR` | `:9840` | listen address |
| `RENSAIGO_CONFIG` / `RENSAIGO_LIBRARY` | `/config` / `/library` | data folder (database, extensions, logs, backups, `secret.key`) and library folder |
| `RENSAIGO_PASSWORD` | (empty) | single password; empty = no login |
| `RENSAIGO_FLARESOLVERR_URL` | (empty) | external FlareSolverr for Cloudflare-protected sources |
| `RENSAIGO_FLARESOLVERR_SESSION` | `rensaigo` | FlareSolverr session name |
| `RENSAIGO_UI_LANG` ⚙ | `en` | interface language: `en` or `es` |
| `RENSAIGO_LANG` ⚙ | `en` | preferred language: filters the available extensions and which languages of a multi-language extension get enabled |
| `RENSAIGO_ADULT` ⚙ | `no` | show +18 extensions and use the +18 version of those that have one |
| `RENSAIGO_DEFAULT_CATEGORY` | `Manga` | folder new series go into (`/library/<Category>/<Series>/`) |
| `RENSAIGO_CHECK_INTERVAL` ⚙ | `2h` | how often followed series are checked for new chapters |
| `RENSAIGO_MAX_ATTEMPTS` ⚙ | `5` | attempts per chapter (backoff: 30 s, 2 min, 8 min, 32 min) |
| `RENSAIGO_DL_CONCURRENCY` / `RENSAIGO_DL_PER_SOURCE` ⚙ | `3` / `1` | chapters at once, in total / per source |
| `RENSAIGO_PAGE_CONCURRENCY` ⚙ | `2` | images at once within a chapter |
| `RENSAIGO_RATE_INTERVAL` ⚙ | `200ms` | minimum gap between requests to the same source |
| `RENSAIGO_EXT_UPDATE_INTERVAL` ⚙ | `24h` | automatic update of the repository index and installed extensions (`0` = never) |
| `RENSAIGO_BACKUP_INTERVAL` / `RENSAIGO_BACKUP_KEEP` ⚙ | `24h` / `2` | compact database backup (`VACUUM INTO`, in `/config/backup`) and how many to keep |
| `RENSAIGO_THUMB_CACHE_MB` ⚙ | `64` | thumbnail cache size |
| `RENSAIGO_LOG_MAX_MB` | `5` | size per log file (current one plus one previous) |
| `RENSAIGO_MEMLIMIT_MIB` | `24` | soft memory limit of the core (`GOMEMLIMIT`) |
| `RENSAIGO_REPO_URL` | Keiyoushi `repo.json` | extension repository |
| `RENSAIGO_REPO_FINGERPRINT` | Keiyoushi | SHA-256 of the certificate that must sign every jar |

## Extension host (JVM)

| Variable | Default | |
|---|---|---|
| `RENSAIGO_HOST_KEEPALIVE` | `90s` | the host shuts down after this long without work |
| `RENSAIGO_HOST_XMX` | `28m` | host max heap |
| `RENSAIGO_HOST_XMX_BOOST` | `96m` | heap a job is retried with if the host runs out of memory (kept for 24 h) |
| `RENSAIGO_HOST_IDLE_GC` | `1` | full GC when the host goes idle |
| `RENSAIGO_HOST_RECYCLE_MB` | `140` | between jobs, restart the host if its RSS is above this (`0` = never) |
| `RENSAIGO_HOST_AOT` | `0` | `1` = JDK 25 AOT cache in `/config` (faster start; first use takes more RAM) |

## Secrets

Tokens (MyAnimeList, AniList, MangaDex), notification channels and the OPDS key are stored encrypted (AES-256-GCM).
The key is `/config/secret.key`: keep it with your backups, without it those settings can't be read and you'd have
to connect the accounts again.
