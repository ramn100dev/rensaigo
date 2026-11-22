// Package store: SQLite (modernc, sin cgo), esquema, eventos y backup único con retención.
package store

import (
	"crypto/cipher"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"rensaigo/internal/i18n"
)

type Store struct {
	DB   *sql.DB
	path string
	aead cipher.AEAD // para los ajustes secretos (ver secrets.go); nil hasta EnableSecrets
}

const schema = `
CREATE TABLE IF NOT EXISTS setting(key TEXT PRIMARY KEY, value TEXT NOT NULL);

-- Índice del repositorio (copia local de index.pb)
CREATE TABLE IF NOT EXISTS repo_ext(
  pkg TEXT PRIMARY KEY, name TEXT, lang TEXT, version TEXT, version_code INT, nsfw INT,
  jar_url TEXT, icon_url TEXT, sources TEXT, content_hash TEXT);

-- Extensiones instaladas (jar verificado en disco)
CREATE TABLE IF NOT EXISTS ext(
  pkg TEXT PRIMARY KEY, name TEXT, version TEXT, version_code INT, jar_path TEXT, jar_sha256 TEXT,
  content_hash TEXT, installed_at INT);

CREATE TABLE IF NOT EXISTS source(
  id TEXT PRIMARY KEY, pkg TEXT NOT NULL, name TEXT, lang TEXT, base_url TEXT,
  enabled INT NOT NULL DEFAULT 1,
  status TEXT NOT NULL DEFAULT 'unknown',  -- ok|cloudflare|webview|javascript|http|network|extension|unknown
  status_msg TEXT NOT NULL DEFAULT '', status_at INT NOT NULL DEFAULT 0);

CREATE TABLE IF NOT EXISTS series(
  id INTEGER PRIMARY KEY, title TEXT NOT NULL, category TEXT NOT NULL, path TEXT NOT NULL UNIQUE,
  author TEXT DEFAULT '', artist TEXT DEFAULT '', description TEXT DEFAULT '', genre TEXT DEFAULT '',
  status INT DEFAULT 0, thumb_url TEXT DEFAULT '',
  paused INT NOT NULL DEFAULT 0, auto_download INT NOT NULL DEFAULT 1,
  imported INT NOT NULL DEFAULT 0, hint_source TEXT DEFAULT '',
  last_check INT NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '', created INT NOT NULL);

-- Una serie puede tener varios sources; manda el de menor "priority"
CREATE TABLE IF NOT EXISTS series_source(
  id INTEGER PRIMARY KEY, series_id INT NOT NULL REFERENCES series(id) ON DELETE CASCADE,
  source_id TEXT NOT NULL, url TEXT NOT NULL, title TEXT, priority INT NOT NULL DEFAULT 0,
  last_check INT NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '',
  UNIQUE(series_id, source_id, url));

CREATE TABLE IF NOT EXISTS chapter(
  id INTEGER PRIMARY KEY, series_id INT NOT NULL REFERENCES series(id) ON DELETE CASCADE,
  number REAL NOT NULL, name TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL,            -- available|queued|downloading|downloaded|failed
  series_source_id INT, url TEXT DEFAULT '', web_url TEXT DEFAULT '', scanlator TEXT DEFAULT '',
  date_upload INT DEFAULT 0, source_name TEXT DEFAULT '', lang TEXT DEFAULT '',
  cbz TEXT DEFAULT '', pages INT DEFAULT 0, bytes INT DEFAULT 0,
  attempts INT NOT NULL DEFAULT 0, next_try INT NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT '',
  created INT NOT NULL, UNIQUE(series_id, number, name));
CREATE INDEX IF NOT EXISTS ix_chapter_state ON chapter(state, next_try);
CREATE UNIQUE INDEX IF NOT EXISTS ix_chapter_cbz ON chapter(cbz) WHERE cbz <> '';

CREATE TABLE IF NOT EXISTS progress(chapter_id INTEGER PRIMARY KEY REFERENCES chapter(id) ON DELETE CASCADE,
  page INT NOT NULL, done INT NOT NULL DEFAULT 0, updated INT NOT NULL);

CREATE TABLE IF NOT EXISTS event(id INTEGER PRIMARY KEY, at INT NOT NULL, kind TEXT NOT NULL, msg TEXT NOT NULL);
`

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	// cache_size negativo = KiB. 2 MiB de caché de páginas bastan y acotan la RAM.
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=cache_size(-2000)" +
		"&_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)&_pragma=wal_autocheckpoint(200)&_pragma=temp_store(FILE)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(3)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("esquema: %w", err)
	}
	// migraciones aditivas (fallan sin consecuencias si la columna ya existe)
	for _, m := range []string{
		`ALTER TABLE series_source ADD COLUMN memo TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE chapter ADD COLUMN memo TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE chapter ADD COLUMN cbz_mtime INT NOT NULL DEFAULT 0`,
		`ALTER TABLE series ADD COLUMN favorite INT NOT NULL DEFAULT 0`,
		`ALTER TABLE source ADD COLUMN has_prefs INT NOT NULL DEFAULT -1`, // -1 = aún no se sabe
		`ALTER TABLE series ADD COLUMN mal_id INT NOT NULL DEFAULT 0`,
		`ALTER TABLE series ADD COLUMN mal_pic TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE series ADD COLUMN mal_total INT NOT NULL DEFAULT 0`,
		`ALTER TABLE series ADD COLUMN mal_read INT NOT NULL DEFAULT 0`,
		// vínculos con servicios de seguimiento (MyAnimeList, AniList, MangaDex). rd = último valor de «leídos»
		// acordado con el servicio: sirve para saber de qué lado ha cambiado
		`CREATE TABLE IF NOT EXISTS tracking(series_id INT NOT NULL REFERENCES series(id) ON DELETE CASCADE,
		  svc TEXT NOT NULL, rid TEXT NOT NULL, total INT NOT NULL DEFAULT 0, rd INT NOT NULL DEFAULT 0,
		  pic TEXT NOT NULL DEFAULT '', PRIMARY KEY(series_id, svc))`,
		// los vínculos de MAL de antes vivían en columnas de series: se pasan a tracking una sola vez
		`INSERT OR IGNORE INTO tracking(series_id, svc, rid, total, rd, pic)
		  SELECT id, 'mal', CAST(mal_id AS TEXT), mal_total, mal_read, mal_pic FROM series WHERE mal_id>0`,
		`UPDATE series SET mal_id=0 WHERE mal_id>0`,
		// sources temporales: rellenan huecos y sus capítulos se sustituyen cuando salen en uno no temporal.
		// from_ch: el source solo aporta capítulos desde ese número (0 = todos)
		`ALTER TABLE series_source ADD COLUMN temporary INT NOT NULL DEFAULT 0`,
		`ALTER TABLE series_source ADD COLUMN from_ch REAL NOT NULL DEFAULT 0`,
		// temp: el archivo en disco viene de un source temporal. upg: hay una sustitución pendiente (el capítulo
		// sigue «downloaded» y legible; series_source_id/url ya apuntan al source que lo sustituye)
		`ALTER TABLE chapter ADD COLUMN temp INT NOT NULL DEFAULT 0`,
		`ALTER TABLE chapter ADD COLUMN upg INT NOT NULL DEFAULT 0`,
		// err_since: desde cuándo falla el source para esta serie (0 = va bien). Para los avisos de la campana
		`ALTER TABLE series_source ADD COLUMN err_since INT NOT NULL DEFAULT 0`,
		`ALTER TABLE ext ADD COLUMN pinned INT NOT NULL DEFAULT 0`, // 1 = «No actualizar»
		`CREATE TABLE IF NOT EXISTS ext_old(pkg TEXT NOT NULL, version TEXT NOT NULL, version_code INT, jar_path TEXT,
			jar_sha256 TEXT, content_hash TEXT, kept_at INT, PRIMARY KEY(pkg, version))`, // versiones anteriores guardadas
		`UPDATE series_source SET err_since=last_check WHERE err_since=0 AND last_error<>''`,
	} {
		db.Exec(m)
	}
	return &Store{DB: db, path: path}, nil
}

func (s *Store) Close() error { return s.DB.Close() }

func (s *Store) Get(key, def string) string {
	var v string
	if s.DB.QueryRow(`SELECT value FROM setting WHERE key=?`, key).Scan(&v) != nil {
		return def
	}
	if strings.HasPrefix(v, encPrefix) {
		p, err := s.open(v)
		if err != nil {
			log.Printf("ajuste %s: %v", key, err)
			return def
		}
		return p
	}
	return v
}

func (s *Store) Set(key, value string) {
	if s.aead != nil && value != "" && isSecret(key) {
		value = s.seal(value)
	}
	s.DB.Exec(`INSERT INTO setting(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
}

// Event guarda un suceso visible en la UI. La tabla se recorta a los últimos 300.
// Event apunta algo en la actividad. El texto se guarda ya traducido al idioma de la interfaz; el tipo (kind) se
// guarda en español y se traduce al mostrarlo.
func (s *Store) Event(kind, format string, args ...any) {
	msg := i18n.Tf(format, args...)
	s.DB.Exec(`INSERT INTO event(at,kind,msg) VALUES(?,?,?)`, time.Now().Unix(), kind, msg)
	s.DB.Exec(`DELETE FROM event WHERE id <= (SELECT max(id) FROM event) - 300`)
}

// Backup deja UNA copia compacta de la BD (VACUUM INTO) y borra las antiguas hasta quedarse con keep.
func (s *Store) Backup(dir string, keep int) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := filepath.Join(dir, "rensaigo-"+time.Now().Format("20060102-150405")+".db")
	tmp := name + ".tmp"
	os.Remove(tmp)
	if _, err := s.DB.Exec(`VACUUM INTO ?`, tmp); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, name); err != nil {
		return "", err
	}
	entries, _ := filepath.Glob(filepath.Join(dir, "rensaigo-*.db"))
	sort.Strings(entries)
	if keep < 1 {
		keep = 1
	}
	for len(entries) > keep {
		os.Remove(entries[0])
		entries = entries[1:]
	}
	// las de cuando se llamaba rensaio-lite: ya hay una nueva
	olds, _ := filepath.Glob(filepath.Join(dir, "rensaio-lite-*.db"))
	for _, o := range olds {
		os.Remove(o)
	}
	// restos de un backup interrumpido
	tmps, _ := filepath.Glob(filepath.Join(dir, "*.tmp"))
	for _, t := range tmps {
		if !strings.HasSuffix(t, filepath.Base(tmp)) {
			os.Remove(t)
		}
	}
	return name, nil
}
