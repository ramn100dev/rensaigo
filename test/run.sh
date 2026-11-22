#!/bin/sh
# Levanta un contenedor de pruebas "rensaigo-t-test" con la imagen indicada sobre testdata/ (red del host para llegar a FlareSolverr).
# Uso: test/run.sh [imagen] [variables extra en formato -e K=V ...]
cd "$(dirname "$0")/.."
IMG=${1:-rensaigo:dev}; [ $# -gt 0 ] && shift
NAME=${RL_NAME:-rensaigo-t-test}; PORT=${RL_PORT:-9842}; DATA=${RL_DATA:-testdata}
docker rm -f $NAME >/dev/null 2>&1
mkdir -p $DATA/config $DATA/library
docker run -d --name $NAME --network host -u "$(id -u):$(id -g)" \
  -v "$PWD/$DATA/config:/config" -v "$PWD/$DATA/library:/library" \
  -e RENSAIGO_ADDR=127.0.0.1:$PORT -e RENSAIGO_FLARESOLVERR_URL=http://127.0.0.1:8191 "$@" "$IMG" >/dev/null
for i in $(seq 1 50); do curl -sf -o /dev/null http://127.0.0.1:$PORT/api/status && break; sleep 0.2; done
curl -sf http://127.0.0.1:$PORT/api/status >/dev/null && echo "$NAME en marcha" || { echo "no arrancó"; docker logs $NAME | tail -20; exit 1; }
