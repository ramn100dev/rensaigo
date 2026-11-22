#!/usr/bin/env python3
"""Test de humo: N extensiones aleatorias del índice, cadena completa en cada una (populares -> detalles/capítulos ->
páginas -> una imagen) y clasificación de fallos. Usa su propio contenedor (rensaigo-smoke, puerto 9841) y su propia
configuración, para no ensuciar la instancia de pruebas.
Uso: smoke.py [--n 100] [--seed 20261113] [--image rensaigo:dev] [--out resultados.json]"""
import argparse, json, os, random, subprocess, sys, time, urllib.parse, urllib.request
ap = argparse.ArgumentParser()
ap.add_argument('--n', type=int, default=100); ap.add_argument('--seed', type=int, default=20261113)
ap.add_argument('--image', default='rensaigo:dev'); ap.add_argument('--out', default='')
ap.add_argument('--flaresolverr', default='http://127.0.0.1:8191')
a = ap.parse_args()
ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), '..', '..'))
CFG, LIB = ROOT + '/testdata/smoke-config', ROOT + '/testdata/smoke-lib'
B = 'http://127.0.0.1:9841'
os.makedirs(CFG, exist_ok=True); os.makedirs(LIB, exist_ok=True)
subprocess.run(['docker', 'rm', '-f', 'rensaigo-t-smoke'], capture_output=True)
subprocess.run(['docker', 'run', '-d', '--name', 'rensaigo-t-smoke', '--network', 'host', '-u', f'{os.getuid()}:{os.getgid()}',
                '-v', CFG + ':/config', '-v', LIB + ':/library', '-e', 'RENSAIGO_ADDR=127.0.0.1:9841', '-e', 'RENSAIGO_HOST_XMX=96m',
                '-e', 'RENSAIGO_FLARESOLVERR_URL=' + a.flaresolverr, '-e', 'RENSAIGO_CHECK_INTERVAL=1000h', a.image], check=True, capture_output=True)
def call(path, data=None, timeout=400):
    body = urllib.parse.urlencode(data).encode() if data is not None else None
    try:
        with urllib.request.urlopen(urllib.request.Request(B + path, data=body, headers={'HX-Request': 'true'}), timeout=timeout) as r:
            return r.status, json.loads(r.read() or b'null')
    except urllib.error.HTTPError as e:
        raw = e.read()
        try: return e.code, json.loads(raw)
        except Exception: return e.code, {'error': raw.decode(errors='replace')[:300]}
    except Exception as e:
        return 0, {'error': repr(e)}
for _ in range(60):
    if call('/api/status')[0] == 200: break
    time.sleep(0.3)
urllib.request.urlopen(urllib.request.Request(B + '/sources/refresh', data=b'', headers={'HX-Request': 'true'}), timeout=120).read()
repo = call('/api/repo')[1]
random.seed(a.seed); sample = random.sample(repo, min(a.n, len(repo)))
rows = []; t0 = time.time()
for i, e in enumerate(sample):
    row = {'pkg': e['pkg'].replace('eu.kanade.tachiyomi.extension.', ''), 'name': e['name'], 'lang': e['lang']}
    st, srcs = call('/api/install', {'pkg': e['pkg']})
    if st != 200 or not srcs:
        row.update(result='instalación', kind=(srcs or {}).get('kind', 'host'), error=str((srcs or {}).get('error'))[:200])
    else:
        st, pr = call('/api/probe', {'source': srcs[0]['id']})
        if st != 200 or not isinstance(pr, dict) or 'ms' not in pr:
            row.update(result='host', kind='host', error=str(pr)[:200])
        elif not pr.get('failed_step'):
            row.update(result='OK', chapters=pr['chapters'], pages=pr['pages'], ms=sum(pr['ms'].values()))
        else:
            row.update(result=pr['kind'], step=pr['failed_step'], error=pr.get('error', '')[:200], popular_ok=pr['mangas'] > 0)
    rows.append(row)
    print(f"{i+1:3d}/{len(sample)} {row['pkg']:<34} {row['result']:<11} {row.get('step','')} {row.get('error','')[:90]}", flush=True)
    if i % 10 == 9:
        s = call('/api/status')[1]; row['host_rss_mb'] = s['host']['rss_kb'] // 1024; row['core_rss_mb'] = s['core_rss_kb'] // 1024
status = call('/api/status')[1]
from collections import Counter
c = Counter(r['result'] for r in rows)
out = {'n': len(rows), 'seed': a.seed, 'counts': dict(c), 'operativas_pct': round(100 * c['OK'] / len(rows), 1),
       'popular_ok': sum(1 for r in rows if r['result'] == 'OK' or r.get('popular_ok')), 'minutes': round((time.time() - t0) / 60, 1),
       'host': status['host'], 'core_rss_mb': status['core_rss_kb'] // 1024, 'rows': rows}
print(json.dumps({k: v for k, v in out.items() if k != 'rows'}, ensure_ascii=False))
if a.out: json.dump(out, open(a.out, 'w'), indent=1, ensure_ascii=False)
subprocess.run(['docker', 'rm', '-f', 'rensaigo-t-smoke'], capture_output=True)
