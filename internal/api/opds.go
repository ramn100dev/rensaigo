package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"rensaigo/internal/library"
)

// OPDS 1.2 para leer la biblioteca desde otras apps (Panels, Chunky, KOReader, Moon+ Reader…). Va con una clave
// en la ruta, como Kavita: la URL se copia tal cual en la app y no hace falta usuario ni contraseña (casi ninguna
// app de lectura sabe de cookies). Incluye Page Streaming (PSE): las apps que lo usan leen página a página sin
// bajarse el CBZ, y lo que leen cuenta aquí como leído.

const (
	opdsNav = "application/atom+xml;profile=opds-catalog;kind=navigation"
	opdsAcq = "application/atom+xml;profile=opds-catalog;kind=acquisition"
)

func (s *Server) opdsKey() string { return s.St.Get("opds.key", "") }

// opdsOK comprueba la clave de la ruta. Sin OPDS activado, todo da 404.
func (s *Server) opdsOK(w http.ResponseWriter, r *http.Request) bool {
	k := s.opdsKey()
	if k == "" || subtle.ConstantTimeCompare([]byte(r.PathValue("key")), []byte(k)) != 1 {
		http.NotFound(w, r)
		return false
	}
	return true
}

func (s *Server) opdsRoutes(mux *http.ServeMux) {
	h := func(f func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if s.opdsOK(w, r) {
				f(w, r, "/opds/"+r.PathValue("key"))
			}
		}
	}
	mux.HandleFunc("GET /opds/{key}", h(s.opdsRoot))
	mux.HandleFunc("GET /opds/{key}/{$}", h(s.opdsRoot))
	mux.HandleFunc("GET /opds/{key}/reading", h(s.opdsReading))
	mux.HandleFunc("GET /opds/{key}/latest", h(s.opdsLatest))
	mux.HandleFunc("GET /opds/{key}/all", h(s.opdsAll))
	mux.HandleFunc("GET /opds/{key}/search", h(s.opdsSearch))
	mux.HandleFunc("GET /opds/{key}/search.xml", h(s.opdsSearchDesc))
	mux.HandleFunc("GET /opds/{key}/series/{id}", h(s.opdsSeries))
	mux.HandleFunc("GET /opds/{key}/cbz/{id}", h(s.opdsCBZ))
	mux.HandleFunc("GET /opds/{key}/page/{id}/{n}", h(s.opdsPage))
	mux.HandleFunc("GET /opds/{key}/thumb/{id}", h(func(w http.ResponseWriter, r *http.Request, _ string) { s.thumb(w, r) }))
	mux.HandleFunc("POST /system/opds", s.opdsToggle)
	mux.HandleFunc("GET /system/opds", s.opdsPanel)
}

// ---------------------------------------------------------------- feeds

func esc(v any) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(fmt.Sprint(v)))
	return strings.ReplaceAll(b.String(), `"`, "&#34;")
}

type feed struct {
	w        http.ResponseWriter
	b        strings.Builder
	base, id string
}

func (s *Server) newFeed(w http.ResponseWriter, base, id, title, self, kind string) *feed {
	f := &feed{w: w, base: base, id: id}
	now := time.Now().UTC().Format(time.RFC3339)
	fmt.Fprintf(&f.b, `<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom" xmlns:opds="http://opds-spec.org/2010/catalog" xmlns:pse="http://vaemendis.net/opds-pse/ns" xmlns:dcterms="http://purl.org/dc/terms/">
<id>urn:rensaigo:%s</id><title>%s</title><updated>%s</updated><author><name>rensaigo</name></author>
<link rel="self" href="%s%s" type="%s"/>
<link rel="start" href="%s" type="%s"/>
<link rel="search" href="%s/search.xml" type="application/opensearchdescription+xml"/>
`, esc(id), esc(title), now, base, self, kind, base, opdsNav, base)
	return f
}

func (f *feed) nav(id, title, href, content string) {
	fmt.Fprintf(&f.b, `<entry><id>urn:rensaigo:%s</id><title>%s</title><updated>%s</updated><content type="text">%s</content>
<link rel="subsection" href="%s%s" type="%s"/></entry>
`, esc(id), esc(title), time.Now().UTC().Format(time.RFC3339), esc(content), f.base, href, opdsAcq)
}

func (f *feed) done() {
	f.b.WriteString("</feed>\n")
	f.w.Header().Set("Content-Type", "application/atom+xml;charset=utf-8")
	io.WriteString(f.w, f.b.String())
}

func (s *Server) opdsRoot(w http.ResponseWriter, r *http.Request, base string) {
	f := s.newFeed(w, base, "root", "rensaigo", "", opdsNav)
	f.nav("reading", "Leyendo", "/reading", "Series empezadas con capítulos sin leer")
	f.nav("latest", "Novedades", "/latest", "Últimos capítulos descargados")
	f.nav("all", "Todas las series", "/all", "La biblioteca entera")
	for _, c := range s.rows(`SELECT category, count(*) n FROM series GROUP BY category ORDER BY category`) {
		f.nav("cat:"+c["category"].(string), c["category"].(string), "/all?cat="+urlq(c["category"].(string)), fmt.Sprint(c["n"], " series"))
	}
	f.done()
}

func urlq(s string) string { return strings.NewReplacer("%", "%25", "&", "%26", "+", "%2B", " ", "%20", "#", "%23", "?", "%3F").Replace(s) }

const opdsSeriesQ = `SELECT s.id, s.title, s.author, s.description,
	(SELECT count(*) FROM chapter WHERE series_id=s.id AND state='downloaded') AS n,
	(SELECT count(*) FROM chapter c JOIN progress p ON p.chapter_id=c.id WHERE c.series_id=s.id AND p.done=1 AND c.state='downloaded') AS rd,
	coalesce((SELECT max(p.updated) FROM chapter c JOIN progress p ON p.chapter_id=c.id WHERE c.series_id=s.id),0) AS last_read
	FROM series s`

func (f *feed) series(rows []M) {
	for _, x := range rows {
		if x["n"].(int64) == 0 {
			continue
		}
		fmt.Fprintf(&f.b, `<entry><id>urn:rensaigo:series:%d</id><title>%s</title><updated>%s</updated>`,
			x["id"], esc(x["title"]), time.Now().UTC().Format(time.RFC3339))
		if a := x["author"]; a != nil && a != "" {
			fmt.Fprintf(&f.b, `<author><name>%s</name></author>`, esc(a))
		}
		fmt.Fprintf(&f.b, `<content type="text">%d capítulos · %d leídos</content>
<link rel="subsection" href="%s/series/%d" type="%s"/>
<link rel="http://opds-spec.org/image" href="%s/thumb/%d" type="image/jpeg"/>
<link rel="http://opds-spec.org/image/thumbnail" href="%s/thumb/%d" type="image/jpeg"/></entry>
`, x["n"], x["rd"], f.base, x["id"], opdsAcq, f.base, x["id"], f.base, x["id"])
	}
}

func (s *Server) opdsAll(w http.ResponseWriter, r *http.Request, base string) {
	q, args := opdsSeriesQ, []any{}
	title := "Todas las series"
	if c := r.FormValue("cat"); c != "" {
		q += ` WHERE s.category=?`
		args = append(args, c)
		title = c
	}
	f := s.newFeed(w, base, "all", title, "/all", opdsNav)
	f.series(s.rows(q+` ORDER BY s.title COLLATE NOCASE`, args...))
	f.done()
}

func (s *Server) opdsReading(w http.ResponseWriter, r *http.Request, base string) {
	f := s.newFeed(w, base, "reading", "Leyendo", "/reading", opdsNav)
	f.series(s.rows(`SELECT * FROM (` + opdsSeriesQ + `) WHERE last_read>0 AND rd<n ORDER BY last_read DESC`))
	f.done()
}

func (s *Server) opdsSearch(w http.ResponseWriter, r *http.Request, base string) {
	q := strings.TrimSpace(r.FormValue("q"))
	f := s.newFeed(w, base, "search", "Buscar: "+q, "/search?q="+urlq(q), opdsNav)
	if q != "" {
		f.series(s.rows(opdsSeriesQ+` WHERE s.title LIKE ? ORDER BY s.title COLLATE NOCASE`, "%"+q+"%"))
	}
	f.done()
}

func (s *Server) opdsSearchDesc(w http.ResponseWriter, r *http.Request, base string) {
	w.Header().Set("Content-Type", "application/opensearchdescription+xml")
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<OpenSearchDescription xmlns="http://a9.com/-/spec/opensearch/1.1/"><ShortName>rensaigo</ShortName><Description>Buscar series</Description>
<InputEncoding>UTF-8</InputEncoding><OutputEncoding>UTF-8</OutputEncoding>
<Url type="%s" template="%s/search?q={searchTerms}"/></OpenSearchDescription>
`, opdsNav, base)
}

const opdsChapterQ = `SELECT c.id, c.series_id, c.number, c.name, c.pages, c.bytes, c.created, s.title AS stitle,
	coalesce(p.page,0) AS page, coalesce(p.done,0) AS done, coalesce(p.updated,0) AS updated
	FROM chapter c JOIN series s ON s.id=c.series_id LEFT JOIN progress p ON p.chapter_id=c.id WHERE c.state='downloaded'`

func (f *feed) chapters(rows []M, withSeries bool) {
	for _, c := range rows {
		t := "Capítulo " + library.FormatNumber(c["number"].(float64))
		if n := c["name"].(string); n != "" && !strings.Contains(n, library.FormatNumber(c["number"].(float64))) {
			t += " · " + n
		} else if n != "" {
			t = n
		}
		if withSeries {
			t = c["stitle"].(string) + " · " + t
		}
		pages, page, done := c["pages"].(int64), c["page"].(int64), c["done"].(int64)
		state := "sin leer"
		switch {
		case done == 1:
			state, page = "leído", pages-1
		case page > 0:
			state = fmt.Sprintf("página %d de %d", page+1, pages)
		}
		upd := time.Unix(max(c["created"].(int64), c["updated"].(int64)), 0).UTC().Format(time.RFC3339)
		fmt.Fprintf(&f.b, `<entry><id>urn:rensaigo:chapter:%d</id><title>%s</title><updated>%s</updated><content type="text">%s</content>
<link rel="http://opds-spec.org/acquisition" href="%s/cbz/%d" type="application/vnd.comicbook+zip" length="%d"/>
<link rel="http://vaemendis.net/opds-pse/stream" href="%s/page/%d/{pageNumber}" type="image/jpeg" pse:count="%d" pse:lastRead="%d"`,
			c["id"], esc(t), upd, state, f.base, c["id"], c["bytes"], f.base, c["id"], pages, page)
		if u := c["updated"].(int64); u > 0 {
			fmt.Fprintf(&f.b, ` pse:lastReadDate="%s"`, time.Unix(u, 0).UTC().Format(time.RFC3339))
		}
		fmt.Fprintf(&f.b, `/>
<link rel="http://opds-spec.org/image/thumbnail" href="%s/thumb/%d" type="image/jpeg"/></entry>
`, f.base, c["series_id"])
	}
}

func (s *Server) opdsSeries(w http.ResponseWriter, r *http.Request, base string) {
	id := pathInt(r, "id")
	var title string
	if s.St.DB.QueryRow(`SELECT title FROM series WHERE id=?`, id).Scan(&title) != nil {
		http.NotFound(w, r)
		return
	}
	f := s.newFeed(w, base, fmt.Sprint("series:", id), title, fmt.Sprint("/series/", id), opdsAcq)
	f.chapters(s.rows(opdsChapterQ+` AND c.series_id=? ORDER BY c.number, c.name`, id), false)
	f.done()
}

func (s *Server) opdsLatest(w http.ResponseWriter, r *http.Request, base string) {
	f := s.newFeed(w, base, "latest", "Novedades", "/latest", opdsAcq)
	f.chapters(s.rows(opdsChapterQ+` ORDER BY c.cbz_mtime DESC, c.id DESC LIMIT 60`), true)
	f.done()
}

// ---------------------------------------------------------------- ficheros

func (s *Server) opdsCBZ(w http.ResponseWriter, r *http.Request, _ string) {
	var cbz string
	if s.St.DB.QueryRow(`SELECT cbz FROM chapter WHERE id=? AND state='downloaded'`, pathInt(r, "id")).Scan(&cbz) != nil {
		http.NotFound(w, r)
		return
	}
	fh, err := os.Open(cbz)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer fh.Close()
	st, _ := fh.Stat()
	w.Header().Set("Content-Type", "application/vnd.comicbook+zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+strings.ReplaceAll(filepath.Base(cbz), `"`, "")+`"`)
	http.ServeContent(w, r, filepath.Base(cbz), st.ModTime(), fh)
}

// opdsPage sirve una página (PSE, desde 0) y apunta el progreso: la app la pide porque se está leyendo. La última
// página marca el capítulo como leído (y sube el progreso a los servicios conectados).
func (s *Server) opdsPage(w http.ResponseWriter, r *http.Request, _ string) {
	id := pathInt(r, "id")
	n, _ := strconv.Atoi(r.PathValue("n"))
	var sid, pages int64
	if s.St.DB.QueryRow(`SELECT series_id, pages FROM chapter WHERE id=? AND state='downloaded'`, id).Scan(&sid, &pages) != nil {
		http.NotFound(w, r)
		return
	}
	s.pageImage(w, r)
	if n == 0 && pages > 1 {
		return // hay apps que piden la primera página como miniatura: abrirla no cuenta como empezar a leer
	}
	done := 0
	if int64(n) >= pages-1 {
		done = 1
	}
	res, err := s.St.DB.Exec(`INSERT INTO progress(chapter_id,page,done,updated) VALUES(?,?,?,?)
		ON CONFLICT(chapter_id) DO UPDATE SET page=max(progress.page, excluded.page), done=max(progress.done, excluded.done), updated=excluded.updated
		WHERE progress.done=0`, id, n, done, time.Now().Unix())
	if err == nil {
		if k, _ := res.RowsAffected(); k > 0 {
			s.touch(sid)
			if done == 1 {
				go s.syncSeries(sid, false)
			}
		}
	}
}

// ---------------------------------------------------------------- ajustes

func (s *Server) opdsPanel(w http.ResponseWriter, r *http.Request) {
	k := s.opdsKey()
	d := M{"On": k != ""}
	if k != "" {
		scheme := "http"
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			scheme = "https"
		}
		d["URL"] = scheme + "://" + r.Host + "/opds/" + k
	}
	s.render(w, "opds_panel", d)
}

// opdsToggle: on (activar con clave nueva si no tenía), off, o new (clave nueva: la URL anterior deja de valer).
func (s *Server) opdsToggle(w http.ResponseWriter, r *http.Request) {
	switch r.FormValue("do") {
	case "off":
		s.St.DB.Exec(`DELETE FROM setting WHERE key='opds.key'`)
	case "on", "new":
		if r.FormValue("do") == "new" || s.opdsKey() == "" {
			b := make([]byte, 12)
			rand.Read(b)
			s.St.Set("opds.key", hex.EncodeToString(b))
		}
	}
	s.opdsPanel(w, r)
}
