#!/bin/sh
# Compila el host dentro de un contenedor Gradle (no hace falta JDK ni Gradle en la máquina).
set -e
cd "$(dirname "$0")"
./vendor.sh
mkdir -p .gradle-home
docker run --rm -u "$(id -u):$(id -g)" -e GRADLE_USER_HOME=/work/.gradle-home -e HOME=/tmp \
  -v "$PWD":/work -w /work gradle:9.7.1-jdk25 gradle --no-daemon -q installDist "$@"
