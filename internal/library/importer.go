package library

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Report resume un escaneo/importación.
type Report struct {
	SeriesNew       int      `json:"series_new"`
	SeriesSeen      int      `json:"series_seen"`
	ChaptersNew     int      `json:"chapters_new"`
	ChaptersSeen    int      `json:"chapters_seen"`
	ChaptersChanged int      `json:"chapters_changed"`
	Ignored         []string `json:"ignored"`         // carpetas con rensaio.json pero sin ningún CBZ
	JSONPhantoms    int      `json:"json_phantoms"`   // archivos que rensaio.json lista y NO existen (no se crean)
	MissingOnDisk   int      `json:"missing_on_disk"` // capítulos de la BD cuyo CBZ ha desaparecido
	Unreadable      []string `json:"unreadable"`
	DurationMillis  int64    `json:"ms"`
}

// rensaioJSON: solo se usa como PISTA de metadatos. Nunca como fuente de qué capítulos existen:
// Rensaio no limpia el JSON al borrar series y sigue listando archivos que ya no están.
type rensaioJSON struct {
	Title       string
	Author      string
	Artist      string
	Description string
	Genre       []string
	Status      int
	Providers   []struct {
		Provider     string `json:"provider"`
		Language     string `json:"language"`
		ThumbnailURL string `json:"thumbnailUrl"`
		Archives     []struct {
			ArchiveName string `json:"archiveName"`
		} `json:"archives"`
	}
}

var (
	fileRe = regexp.MustCompile(`^\[([^\]]+)\]\[([^\]]+)\]\s*(.*?)\s*(?:\(([^)]*)\))?\s*([0-9]+(?:\.[0-9]+)?)?\.cbz$`)
	lastNo = regexp.MustCompile(`([0-9]+(?:\.[0-9]+)?)\D*$`)
)

// Scan recorre <root>/<Categoría>/<Serie>/*.cbz y adopta lo que hay EN DISCO. Es idempotente.
func Scan(db *sql.DB, root string) (*Report, error) {
	t0 := time.Now()
	rep := &Report{}
	cats, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	// Escaneo rápido: lo ya conocido se compara por tamaño y fecha de modificación (un stat por fichero).
	// Solo se abren los CBZ nuevos o cambiados.
	type known struct {
		id          int64
		size, mtime int64
	}
	seen := map[string]known{}
	if rows, err := db.Query(`SELECT id, cbz, bytes, cbz_mtime FROM chapter WHERE cbz<>''`); err == nil {
		for rows.Next() {
			var k known
			var path string
			rows.Scan(&k.id, &path, &k.size, &k.mtime)
			seen[path] = k
		}
		rows.Close()
	}
	for _, cat := range cats {
		if !cat.IsDir() || strings.HasPrefix(cat.Name(), ".") {
			continue
		}
		dirs, _ := os.ReadDir(filepath.Join(root, cat.Name()))
		for _, sd := range dirs {
			if !sd.IsDir() {
				continue
			}
			dir := filepath.Join(root, cat.Name(), sd.Name())
			files, _ := os.ReadDir(dir)
			var cbzs []string
			changed := false
			for _, f := range files {
				if !f.IsDir() && strings.EqualFold(filepath.Ext(f.Name()), ".cbz") {
					cbzs = append(cbzs, f.Name())
					k, ok := seen[filepath.Join(dir, f.Name())]
					if fi, err := f.Info(); !ok || err != nil || fi.Size() != k.size || (k.mtime != 0 && fi.ModTime().Unix() != k.mtime) || k.mtime == 0 {
						changed = true
					}
				}
			}
			var hint rensaioJSON
			hasJSON := false
			if b, err := os.ReadFile(filepath.Join(dir, "rensaio.json")); err == nil {
				hasJSON = json.Unmarshal(b, &hint) == nil
			}
			onDisk := map[string]bool{}
			for _, c := range cbzs {
				onDisk[c] = true
			}
			for _, p := range hint.Providers {
				for _, a := range p.Archives {
					if !onDisk[a.ArchiveName] {
						rep.JSONPhantoms++
					}
				}
			}
			if len(cbzs) == 0 {
				if hasJSON {
					rep.Ignored = append(rep.Ignored, filepath.Join(cat.Name(), sd.Name()))
				}
				continue
			}
			if !changed { // nada nuevo ni modificado en esta carpeta
				rep.SeriesSeen++
				rep.ChaptersSeen += len(cbzs)
				continue
			}
			tx, err := db.Begin()
			if err != nil {
				return nil, err
			}
			var sid int64
			var restore *Sidecar // serie nueva con rensaigo.json: se restaura tras adoptar sus capítulos
			err = tx.QueryRow(`SELECT id FROM series WHERE path=?`, dir).Scan(&sid)
			if err == sql.ErrNoRows {
				restore, _ = ReadSidecar(dir)
				title := hint.Title
				if title == "" {
					title = sd.Name()
				}
				hs, thumb := "", ""
				if len(hint.Providers) > 0 {
					hs, thumb = hint.Providers[0].Provider, hint.Providers[0].ThumbnailURL
				}
				if hs == "" {
					if m := fileRe.FindStringSubmatch(cbzs[0]); m != nil {
						hs = m[1]
					}
				}
				res, err := tx.Exec(`INSERT INTO series(title,category,path,author,artist,description,genre,status,thumb_url,imported,hint_source,paused,created)
					VALUES(?,?,?,?,?,?,?,?,?,1,?,1,?)`, title, cat.Name(), dir, hint.Author, hint.Artist, hint.Description,
					strings.Join(hint.Genre, ", "), hint.Status, thumb, hs, now)
				if err != nil {
					tx.Rollback()
					return nil, err
				}
				sid, _ = res.LastInsertId()
				rep.SeriesNew++
			} else if err != nil {
				tx.Rollback()
				return nil, err
			} else {
				rep.SeriesSeen++
			}
			for _, name := range cbzs {
				path := filepath.Join(dir, name)
				st, err := os.Stat(path)
				if err != nil {
					continue
				}
				if k, ok := seen[path]; ok {
					if k.size == st.Size() && (k.mtime == 0 || k.mtime == st.ModTime().Unix()) {
						if k.mtime == 0 { // fila anterior a la columna de fecha: se anota sin releer
							tx.Exec(`UPDATE chapter SET cbz_mtime=? WHERE id=?`, st.ModTime().Unix(), k.id)
						}
						rep.ChaptersSeen++
						continue
					}
					// el fichero ha cambiado: se relee y se actualiza su fila
					if c, err := OpenCBZ(path, false); err == nil {
						tx.Exec(`UPDATE chapter SET pages=?, bytes=?, cbz_mtime=? WHERE id=?`, len(c.Pages), st.Size(), st.ModTime().Unix(), k.id)
						c.Close()
						rep.ChaptersChanged++
					}
					continue
				}
				c, err := OpenCBZ(path, true)
				if err != nil || len(c.Pages) == 0 {
					if c != nil {
						c.Close()
					}
					rep.Unreadable = append(rep.Unreadable, path)
					continue
				}
				number, chName, src, lang, web := -1.0, "", "", "", ""
				if m := fileRe.FindStringSubmatch(name); m != nil {
					src, lang, chName = m[1], m[2], m[4]
					if m[5] != "" {
						number, _ = strconv.ParseFloat(m[5], 64)
					}
				}
				if ci := c.Info; ci != nil {
					if n, err := strconv.ParseFloat(strings.TrimSpace(ci.Number), 64); err == nil {
						number = n
					}
					if ci.Title != "" {
						chName = ci.Title
					}
					if ci.LanguageISO != "" {
						lang = ci.LanguageISO
					}
					if src == "" {
						src = ci.Translator
					}
					web = ci.Web
				}
				if number < 0 {
					if m := lastNo.FindStringSubmatch(strings.TrimSuffix(name, ".cbz")); m != nil {
						number, _ = strconv.ParseFloat(m[1], 64)
					}
				}
				pages := len(c.Pages)
				c.Close()
				// Si el capítulo ya se conocía (p. ej. "available" tras una comprobación), se adopta el fichero.
				res, _ := tx.Exec(`UPDATE chapter SET state='downloaded', cbz=?, pages=?, bytes=?, cbz_mtime=?, error='' WHERE series_id=? AND number=? AND cbz=''`,
					path, pages, st.Size(), st.ModTime().Unix(), sid, number)
				if n, _ := res.RowsAffected(); n == 0 {
					if _, err := tx.Exec(`INSERT INTO chapter(series_id,number,name,state,web_url,source_name,lang,cbz,pages,bytes,cbz_mtime,created)
						VALUES(?,?,?,'downloaded',?,?,?,?,?,?,?,?)`, sid, number, chName, web, src, lang, path, pages, st.Size(), st.ModTime().Unix(), now); err != nil {
						rep.Unreadable = append(rep.Unreadable, path+": "+err.Error())
						continue
					}
				}
				rep.ChaptersNew++
			}
			if restore != nil {
				applySidecar(tx, sid, dir, restore, now)
			}
			if err := tx.Commit(); err != nil {
				return nil, err
			}
		}
	}
	// Capítulos que la BD da por descargados pero cuyo fichero ya no está: vuelven a "available".
	rows, err := db.Query(`SELECT id, cbz FROM chapter WHERE state='downloaded' AND cbz<>''`)
	if err == nil {
		var gone []int64
		for rows.Next() {
			var id int64
			var p string
			rows.Scan(&id, &p)
			if _, err := os.Stat(p); os.IsNotExist(err) {
				gone = append(gone, id)
			}
		}
		rows.Close()
		for _, id := range gone {
			db.Exec(`UPDATE chapter SET state='available', cbz='', pages=0, bytes=0 WHERE id=?`, id)
		}
		rep.MissingOnDisk = len(gone)
	}
	rep.DurationMillis = time.Since(t0).Milliseconds()
	return rep, nil
}
