# Protocolo núcleo ↔ host (versión 1)

El núcleo Go lanza el host como proceso hijo y habla con él por **stdin/stdout, un objeto JSON por línea** (UTF-8).
Todo lo que no sea protocolo (logs del host y de las extensiones) va a **stderr**.

## Ciclo de vida

1. El núcleo ejecuta `java … -jar rensaigo-host-0.1.jar` cuando llega la primera orden.
2. El host inicializa AndroidCompat y escribe **una** línea de arranque:
   ```json
   {"event":"ready","protocol":1,"init_ms":95,"jvm_uptime_ms":180,"rss_kb":78000,"hwm_kb":78000,"flaresolverr":true}
   ```
   Si no puede arrancar escribe `{"event":"fatal","error":{…}}` y termina con código 2.
3. El núcleo envía órdenes. Cada orden lleva un `id` numérico que el host devuelve en la respuesta.
   **Las órdenes se ejecutan en paralelo** (una corrutina por orden) y las respuestas pueden llegar desordenadas:
   la concurrencia la decide el núcleo.
4. El host termina cuando recibe `{"op":"quit"}` o cuando se cierra stdin. El núcleo lo apaga tras
   `RENSAIGO_HOST_KEEPALIVE` (90 s por defecto) sin órdenes. Si el núcleo muere, el kernel mata al host (`PDEATHSIG`).

## Respuestas

```json
{"id":7,"ok":true,"result":{…},"ms":412}
{"id":8,"ok":false,"error":{"kind":"cloudflare","message":"…","http":403,"trace":"…"},"ms":90}
```

`error.kind` clasifica el fallo para el estado del source:

| kind | significado |
|---|---|
| `cloudflare` | la web pide desafío de Cloudflare y no hay FlareSolverr o no lo resolvió |
| `webview` | la extensión necesita un navegador real (WebView); el host no lleva Chromium |
| `android` | la extensión usa una API de Android que AndroidCompat no implementa (p. ej. `PdfRenderer` para capítulos en PDF) |
| `javascript` | la extensión necesita motor JavaScript (QuickJs) y el host no lo incluye |
| `http` | la web respondió con un código de error (`http` lo indica) |
| `network` | DNS, conexión, TLS o tiempo de espera |
| `extension` | cualquier otra excepción de la extensión (web cambiada, parseo, etc.) |

El supervisor del núcleo añade `host` cuando el fallo es suyo (proceso caído, tiempo agotado).

## Órdenes

| op | parámetros | resultado |
|---|---|---|
| `ping` | — | `{"pong":true}` |
| `load` | `jar` (ruta absoluta; el núcleo ya verificó su firma) | `{"sources":[{"id","name","lang","baseUrl","supportsLatest"}]}` |
| `popular` / `latest` | `source`, `page` (opcional, 1) | `{"hasNext":bool,"mangas":[{"url","title","thumbnail"}]}` |
| `search` | `source`, `query`, `page` | igual que `popular` |
| `details` | `source`, `manga:{url,title}`, `chapters` (opcional, `false` para no pedirlos) | `{"manga":{url,title,thumbnail,author,artist,description,genre,status},"webUrl","chapters":[{"url","name","number","date","scanlator","webUrl"}]}` |
| `pages` | `source`, `chapter:{url,name}` | `{"pages":[{"index","url","imageUrl"}]}` |
| `image` | `source`, `page:{index,url,imageUrl}`, `out` (ruta de destino) | `{"bytes","contentType","imageUrl"}` |
| `prefs` | `source` | `{"prefs":[{index,key,title,summary,kind,valueType,value,default,enabled,visible,entries?,entryValues?}]}` (vacío si la extensión no es configurable) |
| `setpref` | `source`, `key`, `value` (cadena; `true`/`false` para booleanos; lista JSON para conjuntos) | `{"saved":true}`; la extensión se vuelve a instanciar |
| `mem` | `gc` (opcional) | `{"rss_kb","hwm_kb","heap_used_kb","heap_committed_kb","loaded_jars","sources"}` |
| `cookies` | — | `{"cookies":[{"domain","name","expiresAt"}],"userAgent"}` (sin valores; diagnóstico) |
| `quit` | — | (sin respuesta; el host termina) |

Notas:

- `source` es el id del source como **cadena** (es un entero de 64 bits; no cabe en un número JSON).
- `number` es `-1` si la extensión no sabe el número de capítulo; el núcleo lo deduce entonces del nombre.
- `date` va en milisegundos desde época (como en Mihon).
- `image` descarga **siempre con el cliente HTTP de la extensión** (cabeceras, cookies e interceptores de
  descifrado/reordenado). Escribe primero en `<out>.part` y renombra al terminar: un fichero con el nombre final está completo.
  Si la página no trae `imageUrl`, el host la resuelve antes (`getImageUrl`).
- `load` es idempotente por ruta. Tras un reinicio del host, el supervisor vuelve a cargar el jar antes de la
  siguiente orden que lo necesite.

## Entorno del host

| variable / propiedad | uso |
|---|---|
| `-Dsuwayomi.tachidesk.config.server.rootDir=/config/host` | datos persistentes: cookies (`settings/cookie_store.xml`), preferencias de extensiones |
| `RENSAIGO_FLARESOLVERR_URL` | activa el interceptor de Cloudflare contra un FlareSolverr externo |
| `RENSAIGO_FLARESOLVERR_SESSION` | nombre de sesión en FlareSolverr (`rensaigo` por defecto) |
| `RENSAIGO_FLARESOLVERR_TIMEOUT`, `RENSAIGO_FLARESOLVERR_SESSION_TTL` | segundos / minutos |
| `RENSAIGO_HOST_LOG` | nivel de log a stderr (`WARN` por defecto) |
