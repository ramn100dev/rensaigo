package api

import (
	"fmt"
	"html/template"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"rensaigo/internal/i18n"
	"rensaigo/internal/library"
	"rensaigo/internal/safe"
)

// Lector: capítulos, páginas servidas directamente desde el CBZ, portadas y progreso de lectura.

func (s *Server) reader(w http.ResponseWriter, r *http.Request) {
	id := pathInt(r, "id")
	c := s.row(`SELECT c.id, c.series_id, c.number, c.name, c.pages, c.cbz_mtime, s.title, coalesce(p.page,0) AS page, coalesce(p.done,0) AS done
		FROM chapter c JOIN series s ON s.id=c.series_id LEFT JOIN progress p ON p.chapter_id=c.id WHERE c.id=? AND c.state='downloaded'`, id)
	if c == nil {
		http.NotFound(w, r)
		return
	}
	prev := s.row(`SELECT id FROM chapter WHERE series_id=? AND state='downloaded' AND number<? ORDER BY number DESC LIMIT 1`, c["series_id"], c["number"])
	next := s.row(`SELECT id FROM chapter WHERE series_id=? AND state='downloaded' AND number>? ORDER BY number LIMIT 1`, c["series_id"], c["number"])
	s.render(w, "reader", M{"C": c, "Prev": prev, "Next": next})
}

// pageImage sirve la página n directamente desde el CBZ, en streaming (sin extraer a disco ni cargar el fichero).
func (s *Server) pageImage(w http.ResponseWriter, r *http.Request) {
	var cbz string
	if s.St.DB.QueryRow(`SELECT cbz FROM chapter WHERE id=? AND state='downloaded'`, pathInt(r, "id")).Scan(&cbz) != nil {
		http.NotFound(w, r)
		return
	}
	c, err := library.OpenCBZ(cbz, false)
	if err != nil {
		http.Error(w, "cbz ilegible", http.StatusInternalServerError)
		return
	}
	defer c.Close()
	n, _ := strconv.Atoi(r.PathValue("n"))
	if n < 0 || n >= len(c.Pages) {
		http.NotFound(w, r)
		return
	}
	f := c.Pages[n]
	rc, err := f.Open()
	if err != nil {
		http.Error(w, "página ilegible", http.StatusInternalServerError)
		return
	}
	defer rc.Close()
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(f.Name)), ".")
	if ext == "jpg" {
		ext = "jpeg"
	}
	w.Header().Set("Content-Type", "image/"+ext)
	w.Header().Set("Content-Length", strconv.FormatUint(f.UncompressedSize64, 10))
	w.Header().Set("Cache-Control", "private, max-age=604800, immutable")
	io.Copy(w, rc)
}

const placeholder = `<svg xmlns="http://www.w3.org/2000/svg" width="300" height="430"><rect width="100%%" height="100%%" fill="#262a33"/><text x="50%%" y="50%%" fill="#778" font-family="sans-serif" font-size="20" text-anchor="middle">%s</text></svg>`

func (s *Server) thumb(w http.ResponseWriter, r *http.Request) {
	id := pathInt(r, "id")
	if s.trackCover(w, r, id) { // vinculada a MAL/AniList/MangaDex: su portada oficial
		return
	}
	var cbz string
	s.St.DB.QueryRow(`SELECT cbz FROM chapter WHERE series_id=? AND state='downloaded' ORDER BY number LIMIT 1`, id).Scan(&cbz)
	if cbz != "" {
		if p, err := s.Thumbs.Get(cbz); err == nil {
			w.Header().Set("Cache-Control", "private, max-age=86400")
			http.ServeFile(w, r, p)
			return
		}
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "no-cache")
	fmt.Fprintf(w, placeholder, template.HTMLEscapeString(i18n.T("sin portada")))
}

func (s *Server) progress(w http.ResponseWriter, r *http.Request) {
	ch, _ := strconv.ParseInt(r.FormValue("chapter"), 10, 64)
	pg, _ := strconv.Atoi(r.FormValue("page"))
	done := 0
	if r.FormValue("done") == "1" {
		done = 1
	}
	_, err := s.St.DB.Exec(`INSERT INTO progress(chapter_id,page,done,updated) VALUES(?,?,?,?)
		ON CONFLICT(chapter_id) DO UPDATE SET page=excluded.page, done=max(progress.done, excluded.done), updated=excluded.updated`,
		ch, pg, done, time.Now().Unix())
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	var sid int64
	s.St.DB.QueryRow(`SELECT series_id FROM chapter WHERE id=?`, ch).Scan(&sid)
	s.touch(sid)
	if done == 1 { // capítulo terminado: subir el progreso a MyAnimeList
		safe.Go("sincronización", func() { s.syncSeries(sid, false) })
	}
	w.WriteHeader(http.StatusNoContent)
}

// chapterRead marca o desmarca un capítulo como leído desde la lista (toggle). Marcar sube el progreso a MAL;
// desmarcar no lo baja.
func (s *Server) chapterRead(w http.ResponseWriter, r *http.Request) {
	id := pathInt(r, "id")
	var sid int64
	if s.St.DB.QueryRow(`SELECT series_id FROM chapter WHERE id=?`, id).Scan(&sid) != nil {
		http.NotFound(w, r)
		return
	}
	s.touch(sid)
	if r.FormValue("done") == "1" {
		s.St.DB.Exec(`INSERT INTO progress(chapter_id,page,done,updated) VALUES(?,0,1,?)
			ON CONFLICT(chapter_id) DO UPDATE SET done=1, updated=excluded.updated`, id, time.Now().Unix())
		safe.Go("sincronización", func() { s.syncSeries(sid, false) })
	} else {
		s.St.DB.Exec(`DELETE FROM progress WHERE chapter_id=?`, id)
		safe.Go("sincronización", func() { s.syncSeries(sid, true) }) // desmarcar a mano sí baja el contador del servicio
	}
	s.seriesChaptersFor(w, sid)
}
