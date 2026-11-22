#!/usr/bin/env python3
"""Prueba de extremo a extremo contra un rensaigo en marcha.
Cadena por source: buscar -> seguir -> comprobar capítulos -> descargar uno -> aparece en biblioteca -> se lee en el lector.
Uso: e2e.py [--base http://127.0.0.1:9840] [--library DIR] [--pkgs a,b] [--out fichero.json]"""
import argparse, json, os, re, sys, time, urllib.parse, urllib.request, zipfile

ap = argparse.ArgumentParser()
ap.add_argument('--base', default='http://127.0.0.1:9842')
ap.add_argument('--library', default=os.path.join(os.path.dirname(__file__), '..', 'testdata', 'library'))
ap.add_argument('--pkgs', default='es.akaya,es.anzmanga,es.mantrazscan,es.mangamx')
ap.add_argument('--category', default='Pruebas')
ap.add_argument('--out', default='')
a = ap.parse_args()
PRE = 'eu.kanade.tachiyomi.extension.'

class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kw): return None
opener = urllib.request.build_opener(NoRedirect)

def req(path, data=None, timeout=300):
    body = urllib.parse.urlencode(data, doseq=True).encode() if data is not None else None
    r = urllib.request.Request(a.base + path, data=body, headers={'HX-Request': 'true'} if data is not None else {})
    try:
        resp = opener.open(r, timeout=timeout)
        return resp.status, dict(resp.headers), resp.read()
    except urllib.error.HTTPError as e:
        return e.code, dict(e.headers), e.read()

def get_json(path):
    st, _, b = req(path)
    return st, json.loads(b or b'null')

def status(): return get_json('/api/status')[1]

MAGIC = {b'\xff\xd8': 'jpeg', b'\x89P': 'png', b'RI': 'webp', b'GI': 'gif'}
res = {'sources': {}, 'steps': []}
def step(name, ok, detail=''):
    res['steps'].append({'step': name, 'ok': bool(ok), 'detail': str(detail)[:300]})
    print(('OK   ' if ok else 'FALLO') + ' ' + name + (' — ' + str(detail)[:200] if detail else ''), flush=True)
    return ok

st0 = status()
res['status_start'] = st0
st, _, b = req('/sources/refresh', {})
step('repositorio: actualizar índice (huella comprobada)', st == 200, b[:200].decode(errors='replace') if st != 200 else '')

for short in a.pkgs.split(','):
    pkg = PRE + short
    r = res['sources'][short] = {}
    t0 = time.time()
    st, _, b = req('/sources/install', {'pkg': pkg})
    if not step(f'{short}: instalar (firma del jar verificada)', st == 200, b.decode(errors='replace') if st != 200 else f'{time.time()-t0:.1f}s'): continue
    _, srcs = get_json('/api/sources')
    # de una extensión con varios idiomas, el activo (al instalar solo se activa el preferido)
    mine = sorted([s for s in srcs if s['pkg'] == pkg], key=lambda s: -s['enabled'])
    if not step(f'{short}: source registrado', mine): continue
    sid = mine[0]['id']; r['source'] = mine[0]['name']
    t0 = time.time(); st, pop = get_json('/api/search?' + urllib.parse.urlencode({'source': sid, 'q': ''}))
    if not step(f'{short}: populares', st == 200 and pop, pop if st != 200 else f'{len(pop)} resultados en {time.time()-t0:.1f}s'): continue
    m = pop[0]
    word = max(m['title'].split(), key=len)
    t0 = time.time(); st, found = get_json('/api/search?' + urllib.parse.urlencode({'source': sid, 'q': word}))
    step(f'{short}: buscar «{word}»', st == 200 and found, found if st != 200 else f'{len(found)} resultados en {time.time()-t0:.1f}s')
    t0 = time.time()
    st, h, b = req('/series/follow', {'source': sid, 'url': m['url'], 'title': m['title'], 'thumbnail': m.get('thumbnail') or '', 'category': a.category, 'auto': '0', 'memo': json.dumps(m['memo']) if m.get('memo') else ''})
    # la búsqueda de la biblioteca añade la serie sin salir de la página: la respuesta es su tarjeta «añadida»
    ids = [x['id'] for x in get_json('/api/series')[1] if x['title'] == m['title'].strip()]
    if not step(f'{short}: seguir «{m["title"]}»', st == 200 and ids, b.decode(errors='replace')[:300] if not ids else f'{time.time()-t0:.1f}s'): continue
    series = ids[0]; r['series'] = series; r['title'] = m['title']
    _, chs = get_json(f'/api/chapters?series={series}')
    r['chapters'] = len(chs)
    if not step(f'{short}: comprobar capítulos', chs, f'{len(chs)} capítulos'): continue
    ch = max(chs, key=lambda c: c['number']) if short == 'es.anzmanga' else min(chs, key=lambda c: c['number'])
    t0 = time.time()
    req(f'/chapter/{ch["id"]}/retry', {})
    state = '?'
    while time.time() - t0 < 600:
        _, chs2 = get_json(f'/api/chapters?series={series}')
        cur = [c for c in chs2 if c['id'] == ch['id']][0]
        state = cur['state']
        if state in ('downloaded', 'failed'): break
        time.sleep(1)
    r['download_s'] = round(time.time() - t0, 1)
    if not step(f'{short}: descargar capítulo {ch["number"]:g}', state == 'downloaded', cur.get('error') or f'{cur["pages"]} páginas, {cur["bytes"]/1e6:.1f} MB en {r["download_s"]}s'): continue
    r['pages'], r['bytes'] = cur['pages'], cur['bytes']
    # el CBZ en disco: nombre, layout, sin recomprimir, ComicInfo.xml
    rel = cur['cbz'].split('/library/', 1)[-1]
    local = os.path.join(a.library, rel)
    r['cbz'] = rel
    try:
        z = zipfile.ZipFile(local)
        names = z.namelist(); imgs = [i for i in z.infolist() if i.filename != 'ComicInfo.xml']
        okz = z.testzip() is None and 'ComicInfo.xml' in names and len(imgs) == cur['pages'] and all(i.compress_type == zipfile.ZIP_STORED for i in imgs)
        step(f'{short}: CBZ válido en {rel}', okz, f'{len(imgs)} imágenes store, ComicInfo.xml={"ComicInfo.xml" in names}')
        r['comicinfo'] = z.read('ComicInfo.xml').decode()[:600]
    except Exception as e:
        step(f'{short}: CBZ válido', False, repr(e))
    st, _, b = req('/library'); step(f'{short}: aparece en la biblioteca', st == 200 and m['title'][:20].encode() in b.replace(b'&#39;', b"'").replace(b'&amp;', b'&'))
    st, _, b = req(f'/library/{series}'); step(f'{short}: ficha de serie', st == 200 and f'/read/{ch["id"]}'.encode() in b)
    st, _, b = req(f'/read/{ch["id"]}'); step(f'{short}: lector', st == 200 and b'reader.js' in b)
    oks = 0
    for n in range(cur['pages']):
        st, h, b = req(f'/page/{ch["id"]}/{n}')
        if st == 200 and b[:2] in MAGIC and h.get('Content-Type', '').startswith('image/'): oks += 1
    step(f'{short}: páginas servidas desde el CBZ', oks == cur['pages'], f'{oks}/{cur["pages"]}')
    st, h, b = req(f'/thumb/{series}'); step(f'{short}: miniatura', st == 200 and b[:2] == b'\xff\xd8', h.get('Content-Type'))
    st, _, _ = req('/api/progress', {'chapter': ch['id'], 'page': 2, 'done': 0})
    st2, _, b = req(f'/read/{ch["id"]}')  # el lector vuelve a abrir por la página guardada
    step(f'{short}: progreso de lectura guardado', st == 204 and re.search(rb'start:\s*2\b', b) is not None)
    r['ok'] = all(s['ok'] for s in res['steps'] if s['step'].startswith(short + ':'))

# vista unificada y pestañas fusionadas
st, _, b = req('/library'); step('UI: biblioteca unificada (tarjetas, búsqueda en sources, acciones y descargas en la cabecera)', st == 200 and b'data-id=' in b and b'data-act=' in b and b'/series/search' in b and b'/hdr/pill' in b)
step('UI: solo quedan las pestañas Biblioteca, Sources y Sistema', b'href="/import"' not in b and b'href="/series"' not in b and b'href="/sources"' in b and b'href="/system"' in b)
st, h, _ = req('/series'); step('UI: /series redirige a la biblioteca', st == 303 and h.get('Location') == '/library')
st, h, _ = req('/import'); step('UI: /import redirige a Sistema', st == 303 and (h.get('Location') or '').startswith('/system'))
st, _, b = req('/system'); step('UI: Sistema incluye la importación', st == 200 and b'id="importar"' in b and b'/import/scan' in b)
for sname, r in res['sources'].items():
    if r.get('series'):
        for act in ('pause', 'resume', 'retry-failed', 'auto', 'auto'):
            st, _, _ = req(f"/series/{r['series']}/{act}", {})
            if st != 200: step(f'UI: acción {act} del menú', False, st); break
        else: step(f'UI: acciones del menú contextual sobre «{r["title"][:20]}»', True)
        break
res['status_end'] = status()
res['passed'] = sum(1 for s in res['steps'] if s['ok']); res['total'] = len(res['steps'])
print(f"\n{res['passed']}/{res['total']} pasos OK; host: {json.dumps(res['status_end']['host'])}")
if a.out: json.dump(res, open(a.out, 'w'), indent=1, ensure_ascii=False)
sys.exit(0 if res['passed'] == res['total'] else 1)
