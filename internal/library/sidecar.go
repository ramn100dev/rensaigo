package library

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SidecarName: datos de la serie que se guardan junto a sus CBZ. Con ellos la biblioteca se puede llevar a otra
// instalación (o reinstalar) sin perder sources, lo leído ni los vínculos con MAL/AniList/MangaDex. No se usa
// rensaio.json para no pisar el del Rensaio original si comparten carpeta.
const SidecarName = "rensaigo.json"

// legacySidecar: el nombre que tenía antes (rensaio-lite). Se lee si no hay otro y se sustituye al escribir.
const legacySidecar = "rensaio-lite.json"

type Sidecar struct {
	Version     int             `json:"version"`
	Title       string          `json:"title"`
	Author      string          `json:"author,omitempty"`
	Artist      string          `json:"artist,omitempty"`
	Description string          `json:"description,omitempty"`
	Genre       string          `json:"genre,omitempty"`
	Status      int             `json:"status"`
	ThumbURL    string          `json:"thumb_url,omitempty"`
	Paused      bool            `json:"paused"`
	Auto        bool            `json:"auto_download"`
	Favorite    bool            `json:"favorite,omitempty"`
	Sources     []SidecarSource `json:"sources"` // por orden: el primero es el principal
	Tracking    []SidecarTrack  `json:"tracking,omitempty"`
	Read        []string        `json:"read"`               // CBZ terminados
	Progress    map[string]int  `json:"progress,omitempty"` // CBZ empezados: página
	Updated     time.Time       `json:"updated"`
}

type SidecarSource struct {
	SourceID string  `json:"source_id"`
	Name     string  `json:"name"`
	Lang     string  `json:"lang"`
	URL      string  `json:"url"`
	Title    string  `json:"title,omitempty"`
	Memo     string  `json:"memo,omitempty"`
	From     float64 `json:"from,omitempty"`
}

type SidecarTrack struct {
	Svc string `json:"svc"`
	RID string `json:"rid"`
}

// WriteSidecar vuelca los datos de una serie a su carpeta. Solo escribe si algo cambió (no toca el disco en
// balde ni la fecha del fichero).
func WriteSidecar(db *sql.DB, sid int64) error {
	var sc Sidecar
	var dir string
	var paused, auto, fav int
	err := db.QueryRow(`SELECT path, title, author, artist, description, genre, status, thumb_url, paused, auto_download, favorite
		FROM series WHERE id=?`, sid).Scan(&dir, &sc.Title, &sc.Author, &sc.Artist, &sc.Description, &sc.Genre, &sc.Status,
		&sc.ThumbURL, &paused, &auto, &fav)
	if err != nil {
		return err
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return nil // aún sin carpeta (ningún capítulo descargado)
	}
	sc.Version, sc.Paused, sc.Auto, sc.Favorite = 1, paused == 1, auto == 1, fav == 1
	rows, err := db.Query(`SELECT ss.source_id, coalesce(src.name,''), coalesce(src.lang,''), ss.url, coalesce(ss.title,''), ss.memo, ss.from_ch
		FROM series_source ss LEFT JOIN source src ON src.id=ss.source_id WHERE ss.series_id=? ORDER BY ss.priority, ss.id`, sid)
	if err != nil {
		return err
	}
	for rows.Next() {
		var x SidecarSource
		rows.Scan(&x.SourceID, &x.Name, &x.Lang, &x.URL, &x.Title, &x.Memo, &x.From)
		sc.Sources = append(sc.Sources, x)
	}
	rows.Close()
	rows, err = db.Query(`SELECT svc, rid FROM tracking WHERE series_id=? ORDER BY svc`, sid)
	if err != nil {
		return err
	}
	for rows.Next() {
		var x SidecarTrack
		rows.Scan(&x.Svc, &x.RID)
		sc.Tracking = append(sc.Tracking, x)
	}
	rows.Close()
	rows, err = db.Query(`SELECT c.cbz, p.page, p.done FROM progress p JOIN chapter c ON c.id=p.chapter_id
		WHERE c.series_id=? AND c.cbz<>'' ORDER BY c.number, c.cbz`, sid)
	if err != nil {
		return err
	}
	sc.Read = []string{}
	for rows.Next() {
		var cbz string
		var page, done int
		rows.Scan(&cbz, &page, &done)
		if filepath.Dir(cbz) != dir {
			continue
		}
		if done == 1 {
			sc.Read = append(sc.Read, filepath.Base(cbz))
		} else if page > 0 {
			if sc.Progress == nil {
				sc.Progress = map[string]int{}
			}
			sc.Progress[filepath.Base(cbz)] = page
		}
	}
	rows.Close()
	if sc.Sources == nil {
		sc.Sources = []SidecarSource{}
	}
	p := filepath.Join(dir, SidecarName)
	legacy := filepath.Join(dir, legacySidecar)
	_, errNew := os.Stat(p)
	if old, err := ReadSidecar(dir); err == nil && errNew == nil {
		sc.Updated = old.Updated
		a, _ := json.Marshal(old)
		b, _ := json.Marshal(&sc)
		if string(a) == string(b) {
			return nil
		}
	}
	sc.Updated = time.Now().UTC().Truncate(time.Second)
	b, err := json.MarshalIndent(&sc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(p+".tmp", b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(p+".tmp", p); err != nil {
		return err
	}
	os.Remove(legacy)
	return nil
}

func ReadSidecar(dir string) (*Sidecar, error) {
	b, err := os.ReadFile(filepath.Join(dir, SidecarName))
	if errors.Is(err, os.ErrNotExist) {
		b, err = os.ReadFile(filepath.Join(dir, legacySidecar))
	}
	if err != nil {
		return nil, err
	}
	var sc Sidecar
	if err := json.Unmarshal(b, &sc); err != nil {
		return nil, err
	}
	return &sc, nil
}

// applySidecar restaura en una serie recién adoptada lo que guardaba su rensaigo.json: metadatos, estado,
// sources (aunque su extensión aún no esté instalada: empiezan a funcionar al instalarla), vínculos y lo leído.
func applySidecar(tx *sql.Tx, sid int64, dir string, sc *Sidecar, now int64) {
	tx.Exec(`UPDATE series SET title=?, author=?, artist=?, description=?, genre=?, status=?, thumb_url=?,
		paused=?, auto_download=?, favorite=?, imported=? WHERE id=?`,
		sc.Title, sc.Author, sc.Artist, sc.Description, sc.Genre, sc.Status, sc.ThumbURL,
		b2i(sc.Paused), b2i(sc.Auto), b2i(sc.Favorite), b2i(len(sc.Sources) == 0), sid)
	for i, x := range sc.Sources {
		tx.Exec(`INSERT OR IGNORE INTO series_source(series_id,source_id,url,title,priority,memo,from_ch,temporary) VALUES(?,?,?,?,?,?,?,?)`,
			sid, x.SourceID, x.URL, x.Title, i, x.Memo, x.From, b2i(i > 0))
	}
	for _, t := range sc.Tracking {
		tx.Exec(`INSERT OR IGNORE INTO tracking(series_id, svc, rid) VALUES(?,?,?)`, sid, t.Svc, t.RID)
	}
	mark := func(name string, page, done int) {
		tx.Exec(`INSERT OR IGNORE INTO progress(chapter_id,page,done,updated)
			SELECT id, ?, ?, ? FROM chapter WHERE series_id=? AND cbz=?`, page, done, now, sid, filepath.Join(dir, name))
	}
	for _, n := range sc.Read {
		mark(n, 0, 1)
	}
	for n, pg := range sc.Progress {
		if !strings.Contains(n, "/") {
			mark(n, pg, 0)
		}
	}
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
