package api

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"rensaigo/internal/i18n"
	"rensaigo/internal/library"
)

// Biblioteca: la rejilla de series, la ficha de cada serie y lo que se hace con ella en disco.

// QuickScan adopta lo que haya nuevo en la biblioteca. Es barato (un stat por CBZ conocido), así que se lanza al
// arrancar y al entrar en la biblioteca, como mucho una vez cada 20 s.
func (s *Server) QuickScan(force bool) *library.Report {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	if !force && time.Since(s.lastScan) < 20*time.Second {
		return nil
	}
	rep, err := library.Scan(s.St.DB, s.Cfg.LibraryDir)
	s.lastScan = time.Now()
	if err != nil {
		log.Printf("escaneo de biblioteca: %v", err)
		return nil
	}
	b, _ := json.Marshal(rep)
	s.St.Set("last_import", string(b))
	if rep.Offline && !s.offline { // se avisa una vez, no en cada escaneo
		s.St.Event("biblioteca", "no se encuentran casi ninguno de los archivos de %s: ¿el disco está desmontado? No se ha cambiado nada", s.Cfg.LibraryDir)
	}
	s.offline = rep.Offline
	if rep.SeriesNew > 0 || rep.ChaptersNew > 0 || rep.MissingOnDisk > 0 {
		s.St.Event("biblioteca", "escaneo: %d series y %d capítulos nuevos adoptados del disco, %d desaparecidos (%d ms)",
			rep.SeriesNew, rep.ChaptersNew, rep.MissingOnDisk, rep.DurationMillis)
	}
	return rep
}

// libraryIndex: vista unificada. Rejilla de series con el estado de sus sources debajo de cada tarjeta, búsqueda en
// sources para seguir series nuevas y cola de descargas.
func (s *Server) libraryIndex(w http.ResponseWriter, r *http.Request) {
	s.QuickScan(false)
	series := s.cards(`1`)
	genres, srcs := facets(series)
	s.page(w, "library", "library", M{"Series": series, "Genres": genres, "SourceNames": srcs, "Sources": s.enabledSources(), "Cat": s.Cfg.DefaultCat, "Cats": s.categories(), "TrkOn": s.anyTracker()})
}

// libraryGrid: solo las tarjetas, para refrescarlas en vivo (descargas en curso, series recién añadidas).
func (s *Server) libraryGrid(w http.ResponseWriter, r *http.Request) {
	s.render(w, "grid_cards", M{"Series": s.cards(`1`)})
}

// categories: carpetas de primer nivel de la biblioteca (más la categoría por defecto), para elegir dónde guardar.
func (s *Server) categories() []string {
	seen := map[string]bool{s.Cfg.DefaultCat: true}
	cats := []string{s.Cfg.DefaultCat}
	if des, err := os.ReadDir(s.Cfg.LibraryDir); err == nil {
		for _, d := range des {
			if d.IsDir() && !strings.HasPrefix(d.Name(), ".") && !seen[d.Name()] {
				seen[d.Name()] = true
				cats = append(cats, d.Name())
			}
		}
	}
	return cats
}

// librarySeries: ficha única de la serie. A la izquierda la info y el rastreo (sources, acciones); a la derecha los
// capítulos con su estado de descarga y de lectura.
func (s *Server) librarySeries(w http.ResponseWriter, r *http.Request) {
	id := pathInt(r, "id")
	cs := s.cards(`s.id=?`, id)
	if len(cs) == 0 {
		http.NotFound(w, r)
		return
	}
	se := s.row(`SELECT * FROM series WHERE id=?`, id)
	for k, v := range cs[0] {
		se[k] = v
	}
	se["genre_list"] = splitGenres(se["genre"].(string))
	var next M
	if r := s.rows(`SELECT c.id, c.number, coalesce(p.page,0) AS page FROM chapter c LEFT JOIN progress p ON p.chapter_id=c.id
		WHERE c.series_id=? AND c.state='downloaded' AND coalesce(p.done,0)=0 ORDER BY c.number LIMIT 1`, id); len(r) > 0 {
		next = r[0]
	}
	srcs := s.rows(`SELECT ss.id, ss.priority, ss.url, ss.title, ss.last_check, ss.last_error, ss.temporary, ss.from_ch,
		coalesce(src.name, ss.title, '') AS name, coalesce(src.lang,'') AS lang, coalesce(src.status,'missing') AS status,
		coalesce(src.enabled,0) AS enabled, coalesce(src.pkg,'') AS pkg
		FROM series_source ss LEFT JOIN source src ON src.id=ss.source_id WHERE ss.series_id=? ORDER BY ss.priority`, id)
	for _, x := range srcs {
		x["width"] = s.ssWidthDisk(x["id"].(int64))
	}
	s.page(w, "library", "library_series", M{"S": se, "Chapters": s.chapterList(id), "Next": next, "SeriesSources": srcs, "Sources": s.enabledSources(), "Trk": s.trkBoxes(id, nil)})
}

// Estado de rastreo de una serie, con los mismos colores que Rensaio:
// activo (verde) · pausado (amarillo) · sin asignar (ámbar) · completo (azul).
var trackLabel = map[string]string{"active": "Activo", "paused": "Pausado", "unassigned": "Sin asignar", "complete": "Completo"}

// Estado de publicación tal como lo da la extensión (SManga.status).
var pubLabel = map[int64]string{1: "En publicación", 2: "Completado", 3: "Licenciado", 4: "Publicación terminada", 5: "Cancelado", 6: "En pausa"}

func (s *Server) seriesWhere(where string, args ...any) []M {
	return s.rows(`SELECT s.id, s.title, coalesce(s.author,'') AS author, s.category, s.path, s.paused, s.auto_download, s.last_check, s.last_error, s.imported,
		s.favorite, coalesce(s.status,0) AS pub, coalesce(s.genre,'') AS genre,
		(SELECT count(*) FROM chapter c JOIN progress p ON p.chapter_id=c.id WHERE c.series_id=s.id AND p.done=1 AND c.state='downloaded') AS read,
		(SELECT count(*) FROM series_source WHERE series_id=s.id) AS nsources,
		(SELECT count(*) FROM chapter WHERE series_id=s.id) AS total,
		(SELECT count(*) FROM chapter WHERE series_id=s.id AND state='downloaded') AS downloaded,
		(SELECT count(*) FROM chapter WHERE series_id=s.id AND state IN ('queued','downloading')) AS pending,
		(SELECT count(*) FROM chapter WHERE series_id=s.id AND state='failed') AS failed
		FROM series s WHERE `+where+` ORDER BY s.title COLLATE NOCASE`, args...)
}

// cards devuelve las series listas para pintar tarjetas: estado de rastreo, estado de lectura, capítulo en curso y
// posición dentro de él, sources y géneros (para los filtros).
func (s *Server) cards(where string, args ...any) []M {
	series := s.seriesWhere(where, args...)
	if len(series) == 0 {
		return series
	}
	srcs := map[int64][]M{}
	for _, ss := range s.rows(`SELECT ss.series_id, ss.last_error, coalesce(src.name,'?') AS name, coalesce(src.status,'unknown') AS status,
		coalesce(src.enabled,0) AS enabled FROM series_source ss LEFT JOIN source src ON src.id=ss.source_id ORDER BY ss.series_id, ss.priority`) {
		id := ss["series_id"].(int64)
		srcs[id] = append(srcs[id], ss)
	}
	// último capítulo tocado de cada serie
	type pos struct {
		num         float64
		page, pages int64
		done        bool
	}
	last := map[int64]pos{}
	for _, p := range s.rows(`SELECT c.series_id, c.number, c.pages, p.page, p.done FROM progress p JOIN chapter c ON c.id=p.chapter_id
		WHERE c.state='downloaded' ORDER BY p.updated DESC`) {
		id := p["series_id"].(int64)
		if _, ok := last[id]; !ok {
			last[id] = pos{num: toF(p["number"]), page: p["page"].(int64), pages: p["pages"].(int64), done: p["done"].(int64) == 1}
		}
	}
	for _, se := range series {
		id := se["id"].(int64)
		se["sources"] = srcs[id]
		var names []string
		for _, x := range srcs[id] {
			names = append(names, x["name"].(string))
		}
		se["srcnames"] = strings.Join(names, "|")
		se["genres"] = strings.Join(splitGenres(se["genre"].(string)), "|")

		down, read, total := se["downloaded"].(int64), se["read"].(int64), se["total"].(int64)
		pub := se["pub"].(int64)
		switch {
		case se["nsources"].(int64) == 0:
			se["track"] = "unassigned"
		case se["paused"].(int64) == 1:
			se["track"] = "paused"
		case (pub == 2 || pub == 4) && total > 0 && down == total:
			se["track"] = "complete"
		default:
			se["track"] = "active"
		}
		se["track_label"] = i18n.T(trackLabel[se["track"].(string)])
		se["pub_label"] = i18n.T(pubLabel[pub])

		// capítulo en curso: el último abierto; si ya estaba terminado, el siguiente al más alto leído (reabrir uno
		// antiguo para releerlo no hace retroceder la tarjeta)
		p, started := last[id]
		if started && p.done {
			var n float64
			var pages int64
			if s.St.DB.QueryRow(`SELECT number, pages FROM chapter WHERE series_id=? AND state='downloaded' AND number>
				(SELECT coalesce(max(c.number),0) FROM chapter c JOIN progress p ON p.chapter_id=c.id WHERE c.series_id=? AND p.done=1)
				ORDER BY number LIMIT 1`, id, id).Scan(&n, &pages) == nil {
				p = pos{num: n, pages: pages}
			} else if s.St.DB.QueryRow(`SELECT max(c.number) FROM chapter c JOIN progress p ON p.chapter_id=c.id
				WHERE c.series_id=? AND p.done=1 AND c.state='downloaded'`, id).Scan(&n) == nil && n > p.num {
				p.num = n // todo leído: la tarjeta se queda en el último, no en el que reabriste
			}
		}
		if !started {
			var n float64
			if s.St.DB.QueryRow(`SELECT number FROM chapter WHERE series_id=? AND state='downloaded' ORDER BY number LIMIT 1`, id).Scan(&n) == nil {
				p = pos{num: n}
				se["cur"] = true
			}
		} else {
			se["cur"] = true
		}
		se["cur_num"] = strconv.FormatFloat(p.num, 'f', -1, 64)
		pct := 0
		switch {
		case p.done:
			pct = 100
		case p.pages > 1:
			pct = int(p.page * 100 / (p.pages - 1))
		}
		se["cur_pct"] = pct
		// índice del capítulo en curso dentro de la serie (1/36) y posición en la barra: capítulos anteriores
		// completos más la fracción leída del actual
		var idx int64
		if se["cur"] == true {
			s.St.DB.QueryRow(`SELECT count(*) FROM chapter WHERE series_id=? AND number<=?`, id, p.num).Scan(&idx)
		}
		se["cur_idx"] = idx
		pos := 0
		if total > 0 && idx > 0 {
			pos = int((float64(idx-1) + float64(pct)/100) * 100 / float64(total))
		}
		se["cur_pos"] = pos
		switch {
		case down > 0 && read >= down:
			se["reading"] = "read"
		case started:
			se["reading"] = "reading"
		default:
			se["reading"] = "new"
		}
	}
	return series
}

func splitGenres(g string) []string {
	var out []string
	for _, x := range strings.FieldsFunc(g, func(r rune) bool { return r == ',' || r == ';' }) {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

// facets: valores para los desplegables de géneros y sources de la biblioteca.
func facets(series []M) (genres, sources []string) {
	g, src := map[string]bool{}, map[string]bool{}
	for _, se := range series {
		for _, x := range strings.Split(se["genres"].(string), "|") {
			if x != "" {
				g[x] = true
			}
		}
		for _, x := range strings.Split(se["srcnames"].(string), "|") {
			if x != "" {
				src[x] = true
			}
		}
	}
	for k := range g {
		genres = append(genres, k)
	}
	for k := range src {
		sources = append(sources, k)
	}
	sort.Strings(genres)
	sort.Strings(sources)
	return
}

// chapterList: capítulos de la serie con su estado de descarga y de lectura, del más nuevo al más viejo.
func (s *Server) chapterList(id int64) []M {
	// src: de dónde viene lo que hay (el archivo si está descargado); newsrc: el que lo sustituirá, si hay sustitución
	return s.rows(`SELECT c.id, c.number, c.name, c.state, c.pages, c.bytes, c.attempts, c.error, c.upg, c.temp,
		CASE WHEN c.state='downloaded' AND c.source_name<>'' THEN c.source_name ELSE coalesce(src.name, c.source_name, '') END AS src,
		coalesce(src.name, '') AS newsrc, coalesce(p.page,0) AS page, coalesce(p.done,0) AS done
		FROM chapter c LEFT JOIN series_source ss ON ss.id=c.series_source_id LEFT JOIN source src ON src.id=ss.source_id
		LEFT JOIN progress p ON p.chapter_id=c.id
		WHERE c.series_id=? ORDER BY c.number DESC, c.id DESC`, id)
}

// cardResponse responde a una acción lanzada desde una tarjeta repintando solo esa tarjeta.
func (s *Server) cardResponse(w http.ResponseWriter, id int64, toast string) {
	setToast(w, toast)
	cs := s.cards(`s.id=?`, id)
	if len(cs) == 0 {
		w.WriteHeader(http.StatusOK)
		return
	}
	s.render(w, "card", cs[0])
}

// verifySeries abre cada CBZ descargado de la serie. Los que faltan o no se pueden leer vuelven a «disponible» (y a
// la cola, si la serie tiene source y descarga automática).
func (s *Server) verifySeries(id int64) (ok, broken int, err error) {
	type ch struct {
		id    int64
		cbz   string
		pages int
	}
	var chs []ch
	rs, err := s.St.DB.Query(`SELECT id, cbz, pages FROM chapter WHERE series_id=? AND state='downloaded'`, id)
	if err != nil {
		return 0, 0, err
	}
	for rs.Next() {
		var c ch
		rs.Scan(&c.id, &c.cbz, &c.pages)
		chs = append(chs, c)
	}
	rs.Close()
	var bad []int64
	for _, c := range chs {
		z, err := library.OpenCBZ(c.cbz, false)
		if err != nil || len(z.Pages) == 0 {
			bad = append(bad, c.id)
			if z != nil {
				z.Close()
			}
			continue
		}
		z.Close()
		ok++
	}
	for _, cid := range bad {
		s.St.DB.Exec(`UPDATE chapter SET state='available', cbz='', pages=0, bytes=0, cbz_mtime=0 WHERE id=?`, cid)
	}
	if len(bad) > 0 {
		var auto, nsrc int
		s.St.DB.QueryRow(`SELECT auto_download, (SELECT count(*) FROM series_source WHERE series_id=?) FROM series WHERE id=?`, id, id).Scan(&auto, &nsrc)
		if auto == 1 && nsrc > 0 {
			s.Eng.Queue(id, bad)
		}
		s.St.Event("biblioteca", "verificación: %d capítulos ilegibles o desaparecidos vuelven a «disponible»", len(bad))
	}
	return ok, len(bad), nil
}

// deleteSeries quita la serie de la BD y BORRA su carpeta de la biblioteca. Solo se borra una carpeta que esté
// exactamente en <biblioteca>/<Categoría>/<Serie>: nunca la raíz ni una categoría entera.
func (s *Server) deleteSeries(id int64) error {
	var path, title string
	if err := s.St.DB.QueryRow(`SELECT path, title FROM series WHERE id=?`, id).Scan(&path, &title); err != nil {
		return err
	}
	rel, err := filepath.Rel(filepath.Clean(s.Cfg.LibraryDir), filepath.Clean(path))
	safe := err == nil && !strings.HasPrefix(rel, "..") && len(strings.Split(rel, string(filepath.Separator))) == 2
	s.St.DB.Exec(`UPDATE series SET paused=1 WHERE id=?`, id) // que no empiece ninguna descarga mientras tanto
	s.Eng.CancelSeries(id)                                    // y las que están en curso se paran sin escribir nada
	if _, err := s.St.DB.Exec(`DELETE FROM series WHERE id=?`, id); err != nil {
		return err
	}
	if !safe {
		s.St.Event("biblioteca", "«%s» eliminada de la app; su carpeta (%s) no se ha borrado por estar fuera de <biblioteca>/<Categoría>/<Serie>", title, path)
		return nil
	}
	if err := os.RemoveAll(path); err != nil {
		s.St.Event("biblioteca", "«%s» eliminada de la app, pero no se pudo borrar su carpeta: %v", title, err)
		return nil
	}
	s.St.Event("biblioteca", "«%s» eliminada junto con sus archivos", title)
	return nil
}
