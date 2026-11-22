# rensaigo: núcleo Go + host JVM fino de extensiones (bajo demanda).
# Base final con glibc (distroless por defecto; --build-arg BASE=debian:stable-slim si quieres shell para depurar).
# Alpine no vale: la librería nativa de zstd que usa OkHttp no funciona con musl.
ARG BASE=gcr.io/distroless/base-debian12

# Multiplataforma (amd64/arm64): el host (bytecode) y el núcleo Go se compilan en la plataforma de quien construye;
# solo el JRE es de la plataforma destino. Sin BuildKit (builder clásico) se asume linux/amd64.

# ---- host de extensiones (Kotlin/JVM, Gradle con dependencias explícitas)
# HOST_FLAGS=-PwithGraalJs añade el motor JavaScript (GraalJS): +67 MB de imagen y +87 MB de RAM del host cuando se usa.
FROM --platform=${BUILDPLATFORM:-linux/amd64} gradle:9.7.1-jdk25 AS host
WORKDIR /src/host
COPY host/vendor.sh ./
RUN ./vendor.sh
COPY host/settings.gradle.kts host/build.gradle.kts ./
COPY host/src ./src
ARG HOST_FLAGS=""
RUN gradle --no-daemon -q installDist ${HOST_FLAGS}

# ---- runtime Java mínimo (jlink) con su archivo CDS base
FROM eclipse-temurin:25-jdk AS jre
RUN jlink --add-modules java.base,java.xml,java.sql,java.desktop,java.management,java.naming,java.logging,java.prefs,java.net.http,java.scripting,java.instrument,jdk.crypto.ec,jdk.unsupported,jdk.zipfs,jdk.management \
      --strip-debug --no-man-pages --no-header-files --compress=zip-6 --output /opt/jre \
 && /opt/jre/bin/java -Xshare:dump >/dev/null

# ---- núcleo Go (estático, sin cgo)
FROM --platform=${BUILDPLATFORM:-linux/amd64} golang:1.26 AS core
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY web ./web
ARG TARGETARCH
RUN CGO_ENABLED=0 GOARCH=${TARGETARCH:-amd64} go build -trimpath -ldflags="-s -w" -o /out/rensaigo ./cmd/rensaigo \
 && mkdir -p /out/config /out/library

# ---- imagen final
FROM ${BASE}
COPY --from=core /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=jre /opt/jre /opt/jre
COPY --from=host /src/host/build/install/rensaigo-host/lib /opt/host/lib
COPY --from=core /out/rensaigo /usr/local/bin/rensaigo
COPY --from=core --chown=1000:1000 /out/config /config
COPY --from=core --chown=1000:1000 /out/library /library
ENV RENSAIGO_CONFIG=/config RENSAIGO_LIBRARY=/library RENSAIGO_ADDR=:9840 HOME=/config
USER 1000:1000
EXPOSE 9840
VOLUME ["/config", "/library"]
ENTRYPOINT ["/usr/local/bin/rensaigo"]
