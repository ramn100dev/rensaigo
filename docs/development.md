# Development

## Layout

```
cmd/rensaigo/      core main
internal/api/          HTTP, server-rendered UI, reader, OPDS, tracking, notifications, JSON API
internal/store/        SQLite (modernc, no cgo), schema, encrypted settings, backups
internal/library/      CBZ + ComicInfo.xml, thumbnails, importer
internal/extrepo/      repo.json → index → jar; JAR signature verification, versions
internal/hostproc/     host supervisor (on-demand start, keep-alive, restart)
internal/jobs/         source calls, scheduler, download queue, full-chain probe
internal/config, logx  configuration; rotating logs
web/                   HTML templates + htmx (embedded with go:embed; no Node)
host/                  extension host (Kotlin/JVM, Gradle); protocol.md describes the protocol
docker/Dockerfile      multi-stage image (Gradle + jlink + Go → distroless glibc)
test/                  e2e.py, smoke/smoke.py, run.sh
```

## Build and test

```sh
go build ./... && go test ./...        # core
host/build.sh                          # host (Gradle inside a container) → host/build/install/rensaigo-host
test/run.sh                            # test container "rensaigo-t-test" on testdata/
python3 test/e2e.py                    # full chain: search → follow → check → download → library → reader
python3 test/smoke/smoke.py --n 100    # smoke test over random extensions
```

`host/vendor/` (pinned Suwayomi code), `host/build/`, `host/.gradle-home/` and `testdata/` are not versioned:
`host/vendor.sh` downloads the pinned code again and checks its commit and sha256.

Host variants: `host/build.sh -PwithGraalJs` (full JavaScript engine) and `-PwithRhino` (experimental shim).
In the image: `docker build --build-arg HOST_FLAGS=-PwithGraalJs …`.
