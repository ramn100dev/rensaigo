#!/bin/sh
# Descarga el código de Suwayomi-Server fijado (solo se compilan AndroidCompat y la API de sources).
set -e
TAG=v2.4.2366
COMMIT=40bc5da57820324ed0ae724cbbc020cc4673b594
cd "$(dirname "$0")"
STUBS_COMMIT=3983289d9481a8ba99c0b16350b27f41364906a2
STUBS_SHA256=2743be4f4c9ba50f8c86ccc0769e5be5246542488b34e6085a2574ca8ac303e3
D=vendor/m2/com/github/Suwayomi/android-jar/1.0.0
if [ ! -f "$D/android-jar-1.0.0.jar" ]; then
  mkdir -p "$D"
  for f in android-jar-1.0.0.jar android-jar-1.0.0.pom; do
    curl -fsSL -o "$D/$f" "https://github.com/Suwayomi/Suwayomi-Server/raw/$STUBS_COMMIT/com/github/Suwayomi/android-jar/1.0.0/$f"
  done
fi
echo "$STUBS_SHA256  $D/android-jar-1.0.0.jar" | sha256sum -c - >/dev/null || { echo "sha256 inesperado en los stubs de Android" >&2; exit 1; }
[ -d vendor/suwayomi ] && exit 0
mkdir -p vendor
git clone -q --depth 1 --branch "$TAG" https://github.com/Suwayomi/Suwayomi-Server vendor/suwayomi
[ "$(git -C vendor/suwayomi rev-parse HEAD)" = "$COMMIT" ] || { echo "commit inesperado para $TAG" >&2; rm -rf vendor/suwayomi; exit 1; }
rm -rf vendor/suwayomi/.git
