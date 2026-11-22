package api

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"rensaigo/internal/config"
	"rensaigo/internal/i18n"
	"rensaigo/internal/jobs"
	"rensaigo/internal/library"
)

// icon pinta un icono del sprite que va en el layout (#i-<nombre>).
func icon(name string) template.HTML {
	return template.HTML(`<svg class="i" aria-hidden="true"><use href="#i-` + template.HTMLEscapeString(name) + `"/></svg>`)
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

		// capítulo en curso: el último abierto; si se terminó, el siguiente descargado
		p, started := last[id]
		if started && p.done {
			var n float64
			var pages int64
			if s.St.DB.QueryRow(`SELECT number, pages FROM chapter WHERE series_id=? AND state='downloaded' AND number>? ORDER BY number LIMIT 1`,
				id, p.num).Scan(&n, &pages) == nil {
				p = pos{num: n, pages: pages}
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

// setToast pide a la página un aviso (evento htmx "toast"). Las cabeceras HTTP viajan como Latin-1, así que todo lo
// que no es ASCII va escapado como \uXXXX dentro del JSON: si no, las tildes llegan rotas.
func setToast(w http.ResponseWriter, msg string) {
	if msg == "" {
		return
	}
	trigger(w, M{"toast": msg})
}

// libChanged avisa y hace que la biblioteca de detrás del diálogo se recargue sola (evento "libchanged").
func libChanged(w http.ResponseWriter, msg string) {
	trigger(w, M{"toast": msg, "libchanged": true})
}

func trigger(w http.ResponseWriter, ev M) {
	b, _ := json.Marshal(ev)
	var sb strings.Builder
	for _, r := range string(b) {
		if r < 128 {
			sb.WriteRune(r)
		} else if r <= 0xFFFF {
			fmt.Fprintf(&sb, `\u%04x`, r)
		} else {
			r -= 0x10000
			fmt.Fprintf(&sb, `\u%04x\u%04x`, 0xD800+(r>>10), 0xDC00+(r&0x3FF))
		}
	}
	w.Header().Set("HX-Trigger", sb.String())
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

var httpCodeRe = regexp.MustCompile(`HTTP error (\d{3})`)

// friendlyErr traduce los errores típicos de un sitio caído a algo legible; el resto se deja tal cual.
func friendlyErr(err error) string {
	msg := err.Error()
	m := httpCodeRe.FindStringSubmatch(msg)
	if m == nil {
		return msg
	}
	switch c := m[1]; {
	case c >= "520" && c <= "530":
		return i18n.Tf("El sitio no responde (error %s de Cloudflare: su servidor está caído o no contesta). No es cosa de rensaigo: prueba más tarde o usa otro source.", c)
	case c == "502" || c == "503" || c == "504":
		return i18n.Tf("El sitio está caído o saturado (error %s). Prueba más tarde o usa otro source.", c)
	case c == "403":
		return i18n.T("El sitio ha bloqueado la petición (error 403). Si usa Cloudflare, revisa FlareSolverr.")
	case c == "404":
		return i18n.T("El sitio no encuentra la página (error 404): puede que haya cambiado de dominio. Actualiza la extensión o revisa sus preferencias.")
	case c == "429":
		return i18n.T("Demasiadas peticiones al sitio (error 429). Espera un rato o baja las descargas por source.")
	}
	return msg
}

// ---------------------------------------------------------------- cabecera: descargas y actividad

func (s *Server) hdrPill(w http.ResponseWriter, r *http.Request) {
	snap := s.Eng.Snapshot()
	seen, _ := strconv.ParseInt(s.St.Get("events_seen", "0"), 10, 64)
	var unread int
	s.St.DB.QueryRow(`SELECT count(*) FROM event WHERE id>?`, seen).Scan(&unread)
	al := s.alerts()
	lv := ""
	if len(al) > 0 {
		lv = al[0].Level // van ordenados: rojos primero
	}
	s.render(w, "hdr_pill", M{"Active": len(snap.Active), "Queued": snap.Counts["queued"], "Failed": snap.Counts["failed"], "QSeries": snap.QueuedSeries, "Paused": s.Eng.Paused(),
		"Unread": unread + len(al), "Level": lv})
}

func (s *Server) hdrQueue(w http.ResponseWriter, r *http.Request) {
	s.render(w, "hdr_queue", M{"Snap": s.Eng.Snapshot(), "Host": s.Host.Stats(), "Paused": s.Eng.Paused()})
}

// downloadsPause pausa o reanuda todas las descargas (lo que ya está bajando termina).
func (s *Server) downloadsPause(w http.ResponseWriter, r *http.Request) {
	p := !s.Eng.Paused()
	s.Eng.SetPaused(p)
	if p {
		s.St.Event("descarga", "descargas en pausa")
		setToast(w, i18n.T("Descargas en pausa"))
	} else {
		s.St.Event("descarga", "descargas reanudadas")
		setToast(w, i18n.T("Descargas reanudadas"))
	}
	w.Header().Set("HX-Trigger-After-Settle", `{"hdrchanged":true}`)
	s.hdrQueue(w, r)
}

func (s *Server) hdrEvents(w http.ResponseWriter, r *http.Request) {
	ev := s.rows(`SELECT id, at, kind, msg FROM event ORDER BY id DESC LIMIT 40`)
	seen, _ := strconv.ParseInt(s.St.Get("events_seen", "0"), 10, 64)
	for _, e := range ev {
		e["new"] = e["id"].(int64) > seen
		e["kind"] = i18n.T(e["kind"].(string))
	}
	if len(ev) > 0 {
		s.St.Set("events_seen", fmt.Sprint(ev[0]["id"]))
	}
	s.render(w, "hdr_events", M{"Events": ev, "Alerts": s.alerts()})
}

// ---------------------------------------------------------------- iconos de extensiones

var pkgRe = regexp.MustCompile(`^[A-Za-z0-9_.]+$`)

// extIcon sirve el icono de una extensión. Se descarga una vez del repositorio y queda en la caché de /config.
func (s *Server) extIcon(w http.ResponseWriter, r *http.Request) {
	pkg := r.PathValue("pkg")
	if !pkgRe.MatchString(pkg) {
		http.NotFound(w, r)
		return
	}
	p := s.Cfg.Path("cache", "icons", pkg+".png")
	if _, err := os.Stat(p); err != nil {
		var u string
		s.St.DB.QueryRow(`SELECT icon_url FROM repo_ext WHERE pkg=?`, pkg).Scan(&u)
		if !strings.HasPrefix(u, "https://") || fetchTo(r.Context(), u, p) != nil {
			w.Header().Set("Cache-Control", "no-cache")
			http.NotFound(w, r)
			return
		}
	}
	w.Header().Set("Cache-Control", "private, max-age=604800")
	http.ServeFile(w, r, p)
}

func fetchTo(ctx context.Context, url, dest string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", res.StatusCode)
	}
	os.MkdirAll(filepath.Dir(dest), 0o755)
	tmp := dest + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, io.LimitReader(res.Body, 512<<10))
	f.Close()
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}

// ---------------------------------------------------------------- ajustes editables

type settingGroup struct {
	Name  string
	Items []M
}

func (s *Server) settingGroups() []settingGroup {
	var out []settingGroup
	for i := range config.Settings {
		set := &config.Settings[i]
		if len(out) == 0 || out[len(out)-1].Name != i18n.T(set.Group) {
			out = append(out, settingGroup{Name: i18n.T(set.Group)})
		}
		_, custom := s.customSetting(set.Key)
		g := &out[len(out)-1]
		g.Items = append(g.Items, M{"Key": set.Key, "Label": i18n.T(set.Label), "Help": i18n.T(set.Help), "Kind": set.Kind, "Options": s.settingOptions(set), "Value": set.Value(s.Cfg), "Custom": custom})
	}
	return out
}

func (s *Server) customSetting(key string) (string, bool) {
	v := s.St.Get("cfg."+key, "")
	return v, v != ""
}

func (s *Server) settingsSave(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	var errs []string
	changed := 0
	for i := range config.Settings {
		set := &config.Settings[i]
		v, ok := r.Form[set.Key]
		if !ok || strings.TrimSpace(v[0]) == set.Value(s.Cfg) {
			continue
		}
		norm, err := set.Apply(s.Cfg, v[0])
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		s.St.Set("cfg."+set.Key, norm)
		changed++
	}
	s.Thumbs.MaxBytes = int64(s.Cfg.ThumbCacheMB) << 20
	if changed > 0 {
		if prev := i18n.Lang(); prev != s.Cfg.UILang && (prev == "es") != (s.Cfg.UILang == "es") {
			i18n.Set(s.Cfg.UILang)
			w.Header().Set("HX-Refresh", "true") // otro idioma: se recarga la página entera
		}
		s.ApplyAdult()
		s.Eng.Wake()
		log.Printf("ajustes: %d cambiados desde la web", changed)
	}
	msg := ""
	switch {
	case len(errs) > 0:
	case changed > 0:
		msg = i18n.Tf("%d ajustes guardados", changed)
	default:
		msg = i18n.T("Sin cambios")
	}
	s.render(w, "settings_form", M{"Groups": s.settingGroups(), "Errors": errs, "Msg": msg})
}

// jsExts: extensiones de Keiyoushi que ejecutan JavaScript (QuickJs o el desofuscador lib/synchrony), sacadas de
// keiyoushi/extensions-source (búsqueda de código «QuickJs» y dependencias de lib:synchrony, 2026-11-18). Además,
// cualquier source que ya haya fallado con estado «javascript» cuenta como que lo necesita.
var jsExts = map[string]bool{}

func init() {
	for _, p := range []string{"en.mangahere", "en.mangago", "id.mangasusu", "th.doodmanga", "zh.kuaikanmanhua", "zh.comicabc",
		"zh.tencentcomics", "ar.mangatek", "all.mangasincensura", "es.onfmangas", "es.yupmanga", "es.catharsisworld",
		"es.mangasin", "es.mangasnosekai"} {
		jsExts["eu.kanade.tachiyomi.extension."+p] = true
	}
}

func needsJS(pkg, status string) bool { return jsExts[pkg] || status == "javascript" }

func (s *Server) jsInstalled() bool { return s.js().Installed() }

func (s *Server) isProbing(id string) bool {
	s.probeMu.Lock()
	defer s.probeMu.Unlock()
	return s.probing[id]
}

// probeSource prueba un source en segundo plano (petición real de «populares», que deja su estado ok/roto) y
// averigua si tiene preferencias. Como mucho una prueba a la vez por source.
func (s *Server) probeSource(id string) {
	s.probeMu.Lock()
	if s.probing == nil {
		s.probing = map[string]bool{}
	}
	if s.probing[id] {
		s.probeMu.Unlock()
		return
	}
	s.probing[id] = true
	s.probeMu.Unlock()
	go func() {
		defer func() { s.probeMu.Lock(); delete(s.probing, id); s.probeMu.Unlock() }()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		var status string
		s.St.DB.QueryRow(`SELECT status FROM source WHERE id=?`, id).Scan(&status)
		if status == "unknown" {
			s.Eng.Search(ctx, id, "")
		}
		s.checkPrefs(ctx, id)
	}()
}

// checkPrefs guarda si la extensión declara alguna preferencia visible (para mostrar o no su botón).
func (s *Server) checkPrefs(ctx context.Context, id string) {
	res, err := s.Eng.SourceCall(ctx, id, "prefs", nil)
	if err != nil {
		return
	}
	var pr struct {
		Prefs []struct {
			Visible *bool `json:"visible"`
		} `json:"prefs"`
	}
	json.Unmarshal(res, &pr)
	n := 0
	for _, p := range pr.Prefs {
		if p.Visible == nil || *p.Visible {
			n++
		}
	}
	s.St.DB.Exec(`UPDATE source SET has_prefs=? WHERE id=?`, b2i(n > 0), id)
}

// cand: un resultado de búsqueda en un source, con su parecido al título buscado.
type cand struct {
	jobs.Manga
	Source, SourceName, MemoS string
	Score                     float64
}

// searchAll busca uno o varios títulos en todos los sources activos (4 a la vez) y devuelve los resultados
// parecidos ordenados de más a menos parecido, y los sources que no respondieron.
func (s *Server) searchAll(ctx context.Context, titles []string) ([]cand, []string) {
	var (
		mu    sync.Mutex
		wg    sync.WaitGroup
		cands []cand
		fails []string
	)
	score := func(t string) float64 {
		best := 0.0
		for _, ref := range titles {
			best = max(best, titleScore(ref, t))
		}
		return best
	}
	sem := make(chan struct{}, 4)
	for _, sr := range s.enabledSources() {
		wg.Add(1)
		go func(sid, name string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			seen := map[string]bool{}
			failed := true
			for _, q := range titles {
				c, cancel := context.WithTimeout(ctx, 90*time.Second)
				ms, err := s.Eng.Search(c, sid, q)
				cancel()
				if err != nil {
					continue
				}
				failed = false
				hit := false
				mu.Lock()
				for _, m := range ms {
					if sc := score(m.Title); sc >= 0.35 && !seen[m.URL] {
						seen[m.URL] = true
						cands = append(cands, cand{Manga: m, Source: sid, SourceName: name, MemoS: string(m.Memo), Score: sc})
						hit = hit || sc >= 0.85
					}
				}
				mu.Unlock()
				if hit {
					break // ya está en este source: no hace falta probar el título alternativo
				}
			}
			if failed {
				mu.Lock()
				fails = append(fails, name)
				mu.Unlock()
			}
		}(sr["id"].(string), sr["name"].(string))
	}
	wg.Wait()
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].Score > cands[j].Score })
	sort.Strings(fails)
	return cands, fails
}

// settingOptions: valores del desplegable de un ajuste (las carpetas se leen de la biblioteca en cada visita).
func (s *Server) settingOptions(set *config.Setting) []string {
	switch set.Kind {
	case "choice":
		return set.Options
	}
	return nil
}
