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
	Ignored         []string `json:"ignored"`           // carpetas con rensaio.json pero sin ningún CBZ
	JSONPhantoms    int      `json:"json_phantoms"`     // archivos que rensaio.json lista y NO existen (no se crean)
	MissingOnDisk   int      `json:"missing_on_disk"`   // capítulos de la BD cuyo CBZ ha desaparecido
	Offline         bool     `json:"offline,omitempty"` // faltaban casi todos: se da por desmontado y no se toca nada
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
			// Los CBZ se leen ANTES de abrir la transacción: en un disco lento, una carpeta de cientos de capítulos
			// tarda más que el busy_timeout y, con la escritura cogida, cualquier otra («instalar», «seguir»…)
			// fallaría con «database is locked».
			type item struct {
				path               string
				st                 os.FileInfo
				k                  known
				seen, same         bool
				pages              int
				number             float64
				chName, src, lang  string
				web                string
				unreadable, failed bool
			}
			var items []item
			for _, name := range cbzs {
				it := item{path: filepath.Join(dir, name)}
				st, err := os.Stat(it.path)
				if err != nil {
					continue
				}
				it.st = st
				if k, ok := seen[it.path]; ok {
					it.k, it.seen = k, true
					it.same = k.size == st.Size() && (k.mtime == 0 || k.mtime == st.ModTime().Unix())
					if !it.same {
						if c, err := OpenCBZ(it.path, false); err == nil {
							it.pages = len(c.Pages)
							c.Close()
						} else {
							it.failed = true
						}
					}
					items = append(items, it)
					continue
				}
				c, err := OpenCBZ(it.path, true)
				if err != nil || len(c.Pages) == 0 {
					if c != nil {
						c.Close()
					}
					it.unreadable = true
					items = append(items, it)
					continue
				}
				it.number = -1
				if m := fileRe.FindStringSubmatch(name); m != nil {
					it.src, it.lang, it.chName = m[1], m[2], m[4]
					if m[5] != "" {
						it.number, _ = strconv.ParseFloat(m[5], 64)
					}
				}
				if ci := c.Info; ci != nil {
					if n, err := strconv.ParseFloat(strings.TrimSpace(ci.Number), 64); err == nil {
						it.number = n
					}
					if ci.Title != "" {
						it.chName = ci.Title
					}
					if ci.LanguageISO != "" {
						it.lang = ci.LanguageISO
					}
					if it.src == "" {
						it.src = ci.Translator
					}
					it.web = ci.Web
				}
				if it.number < 0 {
					if m := lastNo.FindStringSubmatch(strings.TrimSuffix(name, ".cbz")); m != nil {
						it.number, _ = strconv.ParseFloat(m[1], 64)
					}
				}
				it.pages = len(c.Pages)
				c.Close()
				items = append(items, it)
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
			for _, it := range items {
				st := it.st
				switch {
				case it.seen && it.same:
					if it.k.mtime == 0 { // fila anterior a la columna de fecha: se anota sin releer
						tx.Exec(`UPDATE chapter SET cbz_mtime=? WHERE id=?`, st.ModTime().Unix(), it.k.id)
					}
					rep.ChaptersSeen++
				case it.seen:
					// el fichero ha cambiado: se ha releído y se actualiza su fila
					if !it.failed {
						tx.Exec(`UPDATE chapter SET pages=?, bytes=?, cbz_mtime=? WHERE id=?`, it.pages, st.Size(), st.ModTime().Unix(), it.k.id)
						rep.ChaptersChanged++
					}
				case it.unreadable:
					rep.Unreadable = append(rep.Unreadable, it.path)
				default:
					// Si el capítulo ya se conocía (p. ej. "available" tras una comprobación), se adopta el fichero.
					res, _ := tx.Exec(`UPDATE chapter SET state='downloaded', cbz=?, pages=?, bytes=?, cbz_mtime=?, error='' WHERE series_id=? AND number=? AND cbz=''`,
						it.path, it.pages, st.Size(), st.ModTime().Unix(), sid, it.number)
					if n, _ := res.RowsAffected(); n == 0 {
						if _, err := tx.Exec(`INSERT INTO chapter(series_id,number,name,state,web_url,source_name,lang,cbz,pages,bytes,cbz_mtime,created)
							VALUES(?,?,?,'downloaded',?,?,?,?,?,?,?,?)`, sid, it.number, it.chName, it.web, it.src, it.lang, it.path, it.pages, st.Size(), st.ModTime().Unix(), now); err != nil {
							rep.Unreadable = append(rep.Unreadable, it.path+": "+err.Error())
							continue
						}
					}
					rep.ChaptersNew++
				}
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
		total := 0
		for rows.Next() {
			total++
			var id int64
			var p string
			rows.Scan(&id, &p)
			if _, err := os.Stat(p); os.IsNotExist(err) {
				gone = append(gone, id)
			}
		}
		rows.Close()
		// casi todo «desaparecido» a la vez no es que lo hayas borrado: el disco no está montado o no se puede leer.
		// Entonces no se toca nada (al volver el disco, todo sigue como estaba)
		if len(gone) > 10 && len(gone)*2 > total {
			rep.Offline = true
			gone = nil
		}
		for _, id := range gone {
			db.Exec(`UPDATE chapter SET state='available', cbz='', pages=0, bytes=0 WHERE id=?`, id)
		}
		rep.MissingOnDisk = len(gone)
	}
	rep.DurationMillis = time.Since(t0).Milliseconds()
	return rep, nil
}
