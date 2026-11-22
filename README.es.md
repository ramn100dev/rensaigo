# Rensaigo

**[English](README.md)**

*連載号, «número de la serie por entregas»: el siguiente capítulo de cada serie que sigues, solo.*

Gestor de manga auto-hospedado en un solo contenedor. Sigues una serie y los capítulos nuevos se descargan solos,
sin estar pendiente, desde **extensiones de Mihon/Tachiyomi**. La biblioteca queda como CBZ normales con
`ComicInfo.xml`, y la lees en el lector integrado o en cualquier app OPDS.

Es ligero: un **núcleo Go** siempre encendido (interfaz, API, SQLite, planificador, cola de descargas, lector) y un
**host JVM** fino que ejecuta las extensiones **solo mientras hay trabajo** y luego se apaga. En reposo gasta unas
decenas de MB de RAM. Los sources con Cloudflare pasan por un **FlareSolverr** externo; la imagen no lleva navegador.

## Funciones

- **Biblioteca**: portadas, filtros (seguimiento, lectura, género, source), favoritas, progreso por serie.
- **Varios sources por serie**: uno principal y otros de respaldo, por prioridad. Lo que falta en uno se coge de otro,
  y un capítulo se puede sustituir más tarde por una copia mejor (más resolución) de un source con más prioridad.
- **Lector integrado**: vertical (webtoon) o por páginas, modo manga de derecha a izquierda, recuerda por dónde ibas.
- **Descargas**: cola repartida entre series, reintentos con espera creciente, pausa general, verificación de páginas.
- **Extensiones**: instalación desde el repositorio con verificación de firma, actualización automática, se guardan
  las 2 versiones anteriores para volver a una o fijarla, preferencias de cada extensión, versiones «+18» dentro de
  la normal.
- **Cobertura de sources**: prueba todas las extensiones de un idioma contra *tu* biblioteca (cuántas de tus series
  tiene, cuántos capítulos, resolución de las páginas) y las ordena, para saber cuáles merecen la pena.
- **Seguimiento**: MyAnimeList, AniList y MangaDex (los capítulos leídos y el estado se envían solos).
- **Avisos**: Telegram, ntfy, Discord, Gotify o un webhook cuando llegan capítulos nuevos o algo falla.
- **OPDS** para lectores externos (Panels, Chunky, KOReader, Mihon…).
- **Importar**: adopta una biblioteca de CBZ que ya tengas, incluidos los `rensaio.json` de **Rensaiō / Kaizoku**,
  sin volver a descargar nada.
- Interfaz **adaptada al móvil** (renderizada en el servidor con htmx, sin Node).
- Los secretos (tokens, canales de aviso) se guardan cifrados; copias compactas de la base de datos programadas.

## Arranque rápido

```sh
curl -O https://raw.githubusercontent.com/ramn100dev/rensaigo/main/docker-compose.yml
mkdir -p library
docker compose up -d
```

Abre `http://<servidor>:9840`. Después:

1. **Sources** → instala las extensiones que quieras (el índice del repositorio por defecto ya viene cargado).
2. **Biblioteca** → **Añadir** → busca un título en tus sources y síguelo.
3. ¿Ya tienes biblioteca? **Ajustes → Importar** adopta los CBZ de `/library/<Categoría>/<Serie>/`.

La carpeta de la biblioteca tiene que poder escribirla el usuario del contenedor (`1000:1000` por defecto).

### Docker run

```sh
docker run -d --name rensaigo --restart unless-stopped \
  -p 9840:9840 \
  -e RENSAIGO_FLARESOLVERR_URL=http://flaresolverr:8191 \
  -v rensaigo-config:/config \
  -v /ruta/a/tus/mangas:/library \
  --user 1000:1000 \
  ghcr.io/ramn100dev/rensaigo:latest
```

## Configuración

Casi todo se ajusta desde **Ajustes** en la interfaz (carpeta de descarga, cada cuánto se comprueba, descargas a la
vez, copias…). Las variables de entorno son para lo que hace falta saber antes de arrancar:

| Variable | Por defecto | |
|---|---|---|
| `RENSAIGO_ADDR` | `:9840` | dirección de escucha |
| `RENSAIGO_PASSWORD` | (vacío) | contraseña única; **vacío = sin login**, no lo expongas así |
| `RENSAIGO_FLARESOLVERR_URL` | (vacío) | FlareSolverr para sources con Cloudflare |
| `RENSAIGO_REPO_URL` / `RENSAIGO_REPO_FINGERPRINT` | Keiyoushi | repositorio de extensiones y SHA-256 del certificado que debe firmar sus jars |
| `TZ` | UTC | zona horaria |

Lista completa: [docs/configuration.md](docs/configuration.md).

## Compilar

```sh
docker build -t rensaigo -f docker/Dockerfile .
```

La primera vez tarda unos minutos (Gradle, dependencias y el código fijado de Suwayomi sobre el que va el host).
Notas de desarrollo: [docs/development.md](docs/development.md).

## Licencia

[MIT](LICENSE). Incluye [htmx](https://htmx.org) (0BSD) y la fuente Plus Jakarta Sans ([OFL](web/static/fonts/OFL.txt)).

## Créditos

- Las extensiones se ejecutan sobre **AndroidCompat** de [Suwayomi-Server](https://github.com/Suwayomi/Suwayomi-Server)
  (MPL-2.0), que se descarga fijado al compilar.
- Las extensiones vienen del repositorio de [Keiyoushi](https://github.com/keiyoushi/extensions); no se incluye ninguna.
- Inspirado en [Rensaiō](https://github.com/maxpiva/Rensaio) y Kaizoku. Es un proyecto independiente, no un fork.
