package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"rensaigo/internal/i18n"
	"rensaigo/internal/jobs"
	"rensaigo/internal/safe"
)

// Series: buscar en los sources, seguir series nuevas, añadir sources a una serie y las acciones de su ficha.

func (s *Server) enabledSources() []M {
	return s.rows(`SELECT id, name, lang, status FROM source WHERE enabled=1 ORDER BY name`)
}

func (s *Server) seriesList() []M {
	return s.rows(`SELECT s.id, s.title, s.category, s.paused, s.auto_download, s.last_check, s.last_error, s.imported,
		(SELECT count(*) FROM chapter c JOIN progress p ON p.chapter_id=c.id WHERE c.series_id=s.id AND p.done=1) AS read,
		(SELECT count(*) FROM series_source WHERE series_id=s.id) AS nsources,
		(SELECT count(*) FROM chapter WHERE series_id=s.id) AS total,
		(SELECT count(*) FROM chapter WHERE series_id=s.id AND state='downloaded') AS downloaded,
		(SELECT count(*) FROM chapter WHERE series_id=s.id AND state IN ('queued','downloading')) AS pending,
		(SELECT count(*) FROM chapter WHERE series_id=s.id AND state='failed') AS failed
		FROM series s ORDER BY s.title COLLATE NOCASE`)
}

// La antigua pestaña "Series y descargas" vive ahora dentro de la biblioteca.
func (s *Server) seriesIndex(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/library", http.StatusSeeOther)
}

func (s *Server) seriesSearch(w http.ResponseWriter, r *http.Request) {
	q := r.FormValue("q")
	var srcs []M
	if id := r.FormValue("source"); id != "" && id != "all" {
		srcs = s.rows(`SELECT id, name, lang FROM source WHERE id=? AND enabled=1`, id)
	} else {
		srcs = s.enabledSources()
	}
	type group struct {
		Source M
		Mangas []jobs.Manga
	}
	// en paralelo (4 a la vez): con muchos sources, uno a uno tardaba la suma de todos
	groups := make([]group, len(srcs))
	errs := make([]string, len(srcs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, src := range srcs {
		wg.Add(1)
		go func(i int, src M) {
			defer wg.Done()
			defer safe.Recover("búsqueda")
			sem <- struct{}{}
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
			defer cancel()
			ms, err := s.Eng.Search(ctx, src["id"].(string), q)
			if err != nil {
				errs[i] = src["name"].(string)
			}
			if len(ms) > 30 {
				ms = ms[:30]
			}
			groups[i] = group{Source: src, Mangas: ms}
		}(i, src)
	}
	wg.Wait()
	found := false
	var failed []string
	for i, g := range groups {
		found = found || len(g.Mangas) > 0
		if errs[i] != "" {
			failed = append(failed, errs[i])
		}
	}
	s.render(w, "search_results", M{"Groups": groups, "Found": found, "Failed": strings.Join(failed, ", "),
		"Cat": r.FormValue("category"), "AddTo": r.FormValue("add_to")})
}

// seriesCandidates busca la serie en todos los sources activos que aún no tiene y lista dónde está.
func (s *Server) seriesCandidates(w http.ResponseWriter, r *http.Request) {
	id := pathInt(r, "id")
	var title string
	if s.St.DB.QueryRow(`SELECT title FROM series WHERE id=?`, id).Scan(&title) != nil {
		http.NotFound(w, r)
		return
	}
	titles := []string{title}
	var alt string // el título con el que la conoce su source principal, si es otro
	s.St.DB.QueryRow(`SELECT coalesce(title,'') FROM series_source WHERE series_id=? ORDER BY priority LIMIT 1`, id).Scan(&alt)
	if alt != "" && titleScore(alt, title) < 0.9 {
		titles = append(titles, alt)
	}
	have := map[string]bool{}
	for _, x := range s.rows(`SELECT source_id FROM series_source WHERE series_id=?`, id) {
		have[x["source_id"].(string)] = true
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	all, fails := s.searchAll(ctx, titles)
	var cands []cand // vienen de más a menos parecido: uno por source, el que más se parece
	for _, c := range all {
		if !have[c.Source] && len(cands) < 15 {
			have[c.Source] = true
			cands = append(cands, c)
		}
	}
	pkgs := map[string]string{}
	for _, x := range s.rows(`SELECT id, pkg, lang FROM source`) {
		pkgs[x["id"].(string)] = x["pkg"].(string) + "|" + x["lang"].(string)
	}
	var out []M
	for _, c := range cands {
		pl := strings.SplitN(pkgs[c.Source], "|", 2)
		lang := ""
		if len(pl) == 2 {
			lang = pl[1]
		}
		out = append(out, M{"C": c, "Pkg": pl[0], "Lang": lang, "Same": titleScore(c.Title, title) >= 0.9})
	}
	s.render(w, "src_cands", M{"ID": id, "Cands": out, "Fails": strings.Join(fails, ", ")})
}

func (s *Server) seriesFollow(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	m := jobs.Manga{URL: r.FormValue("url"), Title: r.FormValue("title"), Thumbnail: r.FormValue("thumbnail"), Memo: json.RawMessage(r.FormValue("memo"))}
	var id int64
	var err error
	if addTo, _ := strconv.ParseInt(r.FormValue("add_to"), 10, 64); addTo > 0 {
		// añadir otro source a una serie existente
		id = addTo
		if err = s.Eng.AddSource(id, r.FormValue("source"), m.URL, m.Title, string(m.Memo)); err == nil {
			err = s.Eng.CheckSeries(ctx, id)
		}
	} else {
		id, err = s.Eng.Follow(ctx, r.FormValue("source"), m, r.FormValue("category"), r.FormValue("auto") == "1")
	}
	if err != nil && id == 0 {
		fail(w, 502, err)
		return
	}
	s.touch(id)
	if r.FormValue("add_to") == "" && r.Header.Get("HX-Request") != "" {
		libChanged(w, i18n.Tf("«%s» añadida a la biblioteca", m.Title))
		s.render(w, "follow_done", M{"ID": id, "Title": m.Title, "Thumbnail": m.Thumbnail})
		return
	}
	redirect(w, r, fmt.Sprintf("/library/%d", id))
}

// La antigua página de descargas de la serie es ahora la ficha de la biblioteca.
func (s *Server) seriesDetail(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, fmt.Sprintf("/library/%d", pathInt(r, "id")), http.StatusSeeOther)
}

func (s *Server) seriesChapters(w http.ResponseWriter, r *http.Request) {
	s.seriesChaptersFor(w, pathInt(r, "id"))
}

func (s *Server) seriesAction(w http.ResponseWriter, r *http.Request) {
	id := pathInt(r, "id")
	card := r.Header.Get("X-Card") != ""
	toast := ""
	var err error
	switch r.PathValue("action") {
	case "pause":
		_, err = s.St.DB.Exec(`UPDATE series SET paused=1 WHERE id=?`, id)
		toast = i18n.T("Rastreo pausado")
	case "resume":
		_, err = s.St.DB.Exec(`UPDATE series SET paused=0 WHERE id=?`, id)
		s.Eng.Wake()
		toast = i18n.T("Rastreo reanudado")
	case "fav":
		_, err = s.St.DB.Exec(`UPDATE series SET favorite=1-favorite WHERE id=?`, id)
	case "verify":
		var ok, bad int
		ok, bad, err = s.verifySeries(id)
		if bad > 0 {
			toast = i18n.Tf("%d capítulos correctos · %d ilegibles o desaparecidos, vuelven a «disponible»", ok, bad)
		} else {
			toast = i18n.Tf("Los %d capítulos del disco están bien", ok)
		}
	case "retry-failed":
		_, err = s.St.DB.Exec(`UPDATE chapter SET state='queued', attempts=0, next_try=0, error='' WHERE series_id=? AND (state='failed' OR (state='queued' AND attempts>0))`, id)
		s.Eng.Wake()
	case "auto":
		_, err = s.St.DB.Exec(`UPDATE series SET auto_download=1-auto_download WHERE id=?`, id)
	case "check":
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
		err = s.Eng.CheckSeries(ctx, id)
		cancel()
		toast = i18n.T("Source consultado")
	case "queue":
		var ids []int64
		r.ParseForm()
		for _, v := range r.Form["id"] {
			if n, e := strconv.ParseInt(v, 10, 64); e == nil {
				ids = append(ids, n)
			}
		}
		_, err = s.Eng.Queue(id, ids)
	case "order": // nuevo orden de los sources (arrastrando): el primero es el principal, los demás temporales
		r.ParseForm()
		var ids []int64
		for _, v := range r.Form["ss"] {
			if n, e := strconv.ParseInt(v, 10, 64); e == nil {
				ids = append(ids, n)
			}
		}
		if err = s.Eng.SetOrder(id, ids); err == nil {
			safe.Go("comprobación", func() { s.Eng.CheckSeries(context.Background(), id) })
		}
	case "unsource":
		ssid, _ := strconv.ParseInt(r.FormValue("ss"), 10, 64)
		s.St.DB.Exec(`UPDATE chapter SET series_source_id=NULL, upg=0 WHERE series_source_id=? AND state='downloaded'`, ssid)
		s.St.DB.Exec(`DELETE FROM chapter WHERE series_source_id=? AND state<>'downloaded'`, ssid)
		if _, err = s.St.DB.Exec(`DELETE FROM series_source WHERE id=? AND series_id=?`, ssid, id); err == nil {
			s.Eng.NormSources(id)
			safe.Go("comprobación", func() { s.Eng.CheckSeries(context.Background(), id) }) // lo pendiente que tenía puede estar en otro
		}
	case "from": // el source solo aporta capítulos desde este número
		ssid, _ := strconv.ParseInt(r.FormValue("ss"), 10, 64)
		from, _ := strconv.ParseFloat(strings.Replace(r.FormValue("from"), ",", ".", 1), 64)
		if from < 0 {
			from = 0
		}
		if _, err = s.St.DB.Exec(`UPDATE series_source SET from_ch=? WHERE id=? AND series_id=?`, from, ssid, id); err == nil && from > 0 {
			// lo pendiente de este source por debajo deja de estarlo (si otro source lo tiene, se recupera al comprobar)
			s.St.DB.Exec(`DELETE FROM chapter WHERE series_source_id=? AND state IN ('available','queued','failed') AND number>=0 AND number<?`, ssid, from)
			// y las sustituciones pendientes desde él por debajo se cancelan (el archivo que hay se queda)
			s.St.DB.Exec(`UPDATE chapter SET upg=0, attempts=0, error='' WHERE series_source_id=? AND upg=1 AND number>=0 AND number<?`, ssid, from)
		}
		safe.Go("comprobación", func() { s.Eng.CheckSeries(context.Background(), id) })
	case "delete": // quita la serie Y BORRA su carpeta de la biblioteca
		if err = s.deleteSeries(id); err == nil {
			if card {
				setToast(w, i18n.T("Serie eliminada junto con sus archivos"))
				return
			}
			w.Header().Set("HX-Redirect", "/library")
			return
		}
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		fail(w, 502, err)
		return
	}
	s.touch(id)
	s.dropAlertCache()
	if card {
		s.cardResponse(w, id, toast)
		return
	}
	if a := r.PathValue("action"); a == "order" || a == "from" { // la página ya muestra el cambio: sin recargar
		setToast(w, toast)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("HX-Redirect", r.Header.Get("HX-Current-URL"))
	if r.Header.Get("HX-Request") == "" {
		http.Redirect(w, r, fmt.Sprintf("/library/%d", id), http.StatusSeeOther)
	}
}

func (s *Server) chapterRetry(w http.ResponseWriter, r *http.Request) {
	id := pathInt(r, "id")
	var sid int64
	if s.St.DB.QueryRow(`SELECT series_id FROM chapter WHERE id=?`, id).Scan(&sid) != nil {
		http.NotFound(w, r)
		return
	}
	s.Eng.Queue(sid, []int64{id})
	s.seriesChaptersFor(w, sid)
}

func (s *Server) seriesChaptersFor(w http.ResponseWriter, sid int64) {
	s.render(w, "chapter_rows", M{"Chapters": s.chapterList(sid), "S": s.row(`SELECT * FROM series WHERE id=?`, sid)})
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
			defer safe.Recover("búsqueda")
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
