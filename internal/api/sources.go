package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"rensaigo/internal/config"
	"rensaigo/internal/i18n"
	"rensaigo/internal/safe"
)

// Sources: extensiones instaladas y disponibles, sus preferencias, versiones, idiomas y su estado (ok/rota).

func (s *Server) sourcesIndex(w http.ResponseWriter, r *http.Request) {
	srcs := s.rows(`SELECT s.id, s.pkg, s.name, s.lang, s.base_url, s.enabled, s.status, s.status_msg, s.status_at, s.has_prefs, e.version, e.name AS ext,
		coalesce(r.version,'') AS latest, e.pinned, (SELECT count(*) FROM series_source ss WHERE ss.source_id=s.id) AS nseries
		FROM source s JOIN ext e ON e.pkg=s.pkg LEFT JOIN repo_ext r ON r.pkg=s.pkg ORDER BY s.name, s.lang`)
	ts, _ := strconv.ParseInt(s.St.Get("repo_refreshed", "0"), 10, 64)
	busy := false
	cov := s.covFor(s.Cfg.PrefLang)
	trial := s.covPkg()
	adult := s.adultSiblings() // la versión +18 de una extensión va en la tarjeta de la normal
	withAdult := map[string]bool{}
	for a, base := range adult {
		var on int
		s.St.DB.QueryRow(`SELECT enabled FROM source WHERE id=?`, a).Scan(&on)
		withAdult[base] = on == 1
	}
	// de una extensión con varios idiomas solo se ven los activos y los del idioma preferido
	shownPkg := map[string]bool{}
	for _, src := range srcs {
		if src["enabled"].(int64) == 1 || prefLang(src["lang"].(string), s.Cfg.PrefLang) {
			shownPkg[src["pkg"].(string)] = true
		}
	}
	kept := srcs[:0]
	for _, src := range srcs {
		if src["pkg"] == trial {
			continue // instalada solo para medirla: no se muestra
		}
		if _, ok := adult[src["id"].(string)]; ok {
			continue
		}
		if src["enabled"].(int64) == 0 && !prefLang(src["lang"].(string), s.Cfg.PrefLang) && shownPkg[src["pkg"].(string)] {
			continue
		}
		src["adult"] = withAdult[src["id"].(string)]
		kept = append(kept, src)
	}
	srcs = kept
	for _, src := range srcs {
		src["cov"] = cov[src["pkg"].(string)]
		id := src["id"].(string)
		if src["status"] == "unknown" || src["has_prefs"].(int64) < 0 {
			s.probeSource(id) // las recién instaladas (o de antes de existir has_prefs) se prueban solas
		}
		src["probing"] = s.isProbing(id)
		busy = busy || src["probing"].(bool)
		src["js"] = needsJS(src["pkg"].(string), src["status"].(string))
	}
	js := ""
	if !s.jsInstalled() {
		js = "no" // sin motor JS instalado, de entrada se ocultan las que lo necesitan
	}
	data := s.repoData("", s.Cfg.PrefLang, js)
	data["Langs"] = config.Langs
	data["Sources"], data["RepoAt"], data["Busy"] = srcs, ts, busy
	s.page(w, "sources", "sources", data)
}

// repoData: extensiones del índice del repositorio que aún no están instaladas, filtradas por texto e idioma.
func (s *Server) repoData(q, lang, js string) M {
	like := "%" + strings.TrimSpace(q) + "%"
	args := []any{like, like}
	where := `(r.name LIKE ? OR r.pkg LIKE ?) AND e.pkg IS NULL`
	if !s.adultAllowed() {
		where += ` AND r.nsfw<>3`
	}
	if lang != "" { // las multidioma («all») también: suelen tener ese idioma
		where += ` AND (r.lang=? OR r.lang='all')`
		args = append(args, lang)
	}
	all := s.rows(`SELECT r.pkg, r.name, r.lang, r.version, r.nsfw FROM repo_ext r LEFT JOIN ext e ON e.pkg=r.pkg
		WHERE `+where+` ORDER BY r.name COLLATE NOCASE LIMIT 400`, args...)
	var rows []M
	for _, x := range all {
		x["js"] = needsJS(x["pkg"].(string), "")
		if js == "no" && x["js"].(bool) {
			continue
		}
		rows = append(rows, x)
	}
	// las medidas primero, de mejor a peor; luego las que faltan por medir, por nombre
	cov := s.covFor(lang)
	for _, x := range rows {
		if c, ok := cov[x["pkg"].(string)]; ok {
			x["cov"] = c
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		ci, oki := rows[i]["cov"].(M)
		cj, okj := rows[j]["cov"].(M)
		if oki != okj {
			return oki
		}
		return oki && ci["score"].(float64) > cj["score"].(float64)
	})
	var n int
	s.St.DB.QueryRow(`SELECT count(*) FROM repo_ext`).Scan(&n)
	return M{"Rows": rows, "HasIndex": n > 0, "Lang": lang, "JS": js, "Measured": len(cov)}
}

func (s *Server) sourcesRepo(w http.ResponseWriter, r *http.Request) {
	s.render(w, "repo_cards", s.repoData(r.FormValue("q"), r.FormValue("lang"), r.FormValue("js")))
}

func (s *Server) sourcesRefresh(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	n, err := s.Repo.Refresh(ctx)
	if err != nil {
		fail(w, 502, err)
		return
	}
	s.St.Event("repositorio", "índice actualizado: %d extensiones", n)
	w.Header().Set("HX-Redirect", "/sources")
}

func (s *Server) sourcesInstall(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	infos, err := s.Repo.Install(ctx, r.FormValue("pkg"))
	if err != nil {
		fail(w, 502, err)
		return
	}
	// una extensión con muchos idiomas (MangaDex trae 60): solo se activan los del idioma preferido, si tiene alguno
	if len(infos) > 1 {
		match := false
		for _, in := range infos {
			match = match || prefLang(in.Lang, s.Cfg.PrefLang)
		}
		if match {
			for _, in := range infos {
				if !prefLang(in.Lang, s.Cfg.PrefLang) {
					s.St.DB.Exec(`UPDATE source SET enabled=0 WHERE id=?`, in.ID)
				}
			}
		}
	}
	s.ApplyAdult()
	adult := s.adultSiblings()
	for _, in := range infos {
		var on int
		s.St.DB.QueryRow(`SELECT enabled FROM source WHERE id=?`, in.ID).Scan(&on)
		if _, ok := adult[in.ID]; !ok && on == 1 {
			s.probeSource(in.ID) // se prueba sola: no hace falta pulsar «probar»
		}
	}
	w.Header().Set("HX-Redirect", "/sources")
}

// prefLang: el idioma de un source vale para el preferido («es» incluye «es-419»; «all» lo incluye todo).
func prefLang(lang, pref string) bool { return pref == "all" || langMatch(lang, pref) }

func (s *Server) sourcesUninstall(w http.ResponseWriter, r *http.Request) {
	if err := s.Repo.Uninstall(r.FormValue("pkg")); err != nil {
		fail(w, 409, err)
		return
	}
	s.Host.Stop() // el host no descarga clases: se reinicia sin ella en la siguiente petición
	w.Header().Set("HX-Redirect", "/sources")
}

func (s *Server) sourcesToggle(w http.ResponseWriter, r *http.Request) {
	s.St.DB.Exec(`UPDATE source SET enabled=1-enabled WHERE id=?`, r.PathValue("id"))
	s.ApplyAdult() // su versión +18 (si tiene) sigue a la normal
	s.Eng.Wake()
	w.Header().Set("HX-Redirect", "/sources")
}

// sourcesTest lanza una petición real ("populares") para refrescar el estado ok/roto.
func (s *Server) sourcesTest(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	id := r.PathValue("id")
	_, err := s.Eng.Search(ctx, id, "")
	s.checkPrefs(ctx, id)
	var name string
	s.St.DB.QueryRow(`SELECT name FROM source WHERE id=?`, id).Scan(&name)
	if err != nil {
		setToast(w, i18n.Tf("%s: no responde · %s", name, trunc(err.Error(), 120)))
	} else {
		setToast(w, i18n.Tf("%s: funciona", name))
	}
	s.sourcesIndex(w, r) // el botón se queda con #installed (hx-select): se ve el estado nuevo sin recargar
}

// sourcePrefs muestra las preferencias que declara la propia extensión (dominio alternativo, calidad, etc.).
func (s *Server) sourcePrefs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	var pr struct {
		Prefs []M `json:"prefs"`
	}
	var hasPrefs int
	var pkg, cur string
	var pinned int
	s.St.DB.QueryRow(`SELECT s.has_prefs, s.pkg, e.version, e.pinned FROM source s JOIN ext e ON e.pkg=s.pkg WHERE s.id=?`, id).
		Scan(&hasPrefs, &pkg, &cur, &pinned)
	prefErr := ""
	if hasPrefs != 0 { // sin preferencias no hace falta arrancar el host
		res, err := s.Eng.SourceCall(ctx, id, "prefs", nil)
		if err != nil {
			prefErr = err.Error()
		}
		json.Unmarshal(res, &pr)
	}
	for _, p := range pr.Prefs { // conjunto de valores marcados, para la plantilla
		sel := map[string]bool{}
		if vs, ok := p["value"].([]any); ok {
			for _, v := range vs {
				sel[fmt.Sprint(v)] = true
			}
		}
		p["selected"] = sel
		if p["value"] == nil {
			p["value"] = p["default"]
		}
	}
	var latest string
	s.St.DB.QueryRow(`SELECT version FROM repo_ext WHERE pkg=?`, pkg).Scan(&latest)
	var adult M
	if a := s.adultOf(id); a != "" {
		var used int
		s.St.DB.QueryRow(`SELECT count(*) FROM series_source WHERE source_id=?`, a).Scan(&used)
		adult = M{"On": s.adultFor(id), "Used": used}
	}
	s.render(w, "source_prefs", M{"ID": id, "Prefs": pr.Prefs, "Saved": r.FormValue("saved"), "PrefErr": prefErr, "Adult": adult, "Langs": s.langSiblings(id),
		"Ver": M{"Cur": cur, "Old": s.Repo.OldVersions(pkg), "Pinned": pinned == 1, "Latest": latest}})
}

// sourceVersion: elegir otra versión guardada de la extensión y fijarla (o soltarla para que se actualice).
func (s *Server) sourceVersion(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var pkg, cur string
	if s.St.DB.QueryRow(`SELECT s.pkg, e.version FROM source s JOIN ext e ON e.pkg=s.pkg WHERE s.id=?`, id).Scan(&pkg, &cur) != nil {
		http.NotFound(w, r)
		return
	}
	pin := r.FormValue("pin") == "1"
	if v := r.FormValue("version"); v != "" && v != cur {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		err := s.Repo.UseVersion(ctx, pkg, v)
		cancel()
		if err != nil {
			fail(w, 502, err)
			return
		}
		setToast(w, i18n.Tf("Ahora usa la versión %s (fijada)", v))
		w.Header().Set("HX-Trigger-After-Settle", `{"srcchanged":true}`)
	} else {
		s.Repo.SetPinned(pkg, pin)
		if pin {
			setToast(w, i18n.T("Fijada: no se actualizará sola"))
		} else {
			setToast(w, i18n.T("Se actualizará con las demás"))
		}
		w.Header().Set("HX-Trigger-After-Settle", `{"srcchanged":true}`)
	}
	s.sourcePrefs(w, r)
}

// langSiblings: los idiomas de una extensión que trae varios (MangaDex trae 60), cada uno un source aparte. Nil si
// solo tiene uno. No cuenta las versiones +18, que van con su normal.
func (s *Server) langSiblings(id string) []M {
	adult := s.adultSiblings()
	var out []M
	for _, x := range s.rows(`SELECT b.id, b.lang, b.enabled, (SELECT count(*) FROM series_source ss WHERE ss.source_id=b.id) AS used
		FROM source a JOIN source b ON b.pkg=a.pkg WHERE a.id=? ORDER BY b.lang`, id) {
		if _, ok := adult[x["id"].(string)]; !ok {
			out = append(out, x)
		}
	}
	if len(out) < 2 {
		return nil
	}
	return out
}

// sourceLang activa o desactiva un idioma de la extensión desde su modal. Uno que ya usa alguna serie no se quita.
func (s *Server) sourceLang(w http.ResponseWriter, r *http.Request) {
	sib := r.FormValue("sib")
	var on, used int
	if s.St.DB.QueryRow(`SELECT b.enabled, (SELECT count(*) FROM series_source ss WHERE ss.source_id=b.id)
		FROM source a JOIN source b ON b.pkg=a.pkg WHERE a.id=? AND b.id=?`, r.PathValue("id"), sib).Scan(&on, &used) != nil {
		http.NotFound(w, r)
		return
	}
	if on == 1 && used > 0 {
		fail(w, 409, i18n.Errorf("Lo usa alguna serie: quítalo de ella primero"))
		return
	}
	s.St.DB.Exec(`UPDATE source SET enabled=? WHERE id=?`, 1-on, sib)
	s.ApplyAdult()
	if on == 0 {
		s.probeSource(sib)
		s.Eng.Wake()
	}
	w.Header().Set("HX-Trigger-After-Settle", `{"srcchanged":true}`)
	s.sourcePrefs(w, r)
}

func (s *Server) sourcePrefSet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	r.ParseForm()
	if r.FormValue("key") == "rensaigo.adult" { // nuestro, no de la extensión: usar o no su versión +18
		on := r.FormValue("value") == "true"
		s.St.Set("adult.src."+id, map[bool]string{true: "1", false: "0"}[on])
		s.ApplyAdult()
		w.Header().Set("HX-Trigger-After-Settle", `{"srcchanged":true}`)
		s.sourcePrefs(w, r)
		return
	}
	value := r.FormValue("value")
	switch r.FormValue("type") {
	case "Boolean":
		value = strconv.FormatBool(value == "true" || value == "on")
	case "Set<String>":
		b, _ := json.Marshal(append([]string{}, r.Form["value"]...))
		value = string(b)
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	if _, err := s.Eng.SourceCall(ctx, id, "setpref", M{"key": r.FormValue("key"), "value": value}); err != nil {
		fail(w, 502, err)
		return
	}
	r.Form.Set("saved", r.FormValue("key"))
	s.sourcePrefs(w, r)
}

func (s *Server) sourcesUpdateAll(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	n, err := s.Repo.UpdateInstalled(ctx)
	if err != nil {
		fail(w, 502, err)
		return
	}
	s.St.Event("extensión", "actualización manual: %d extensiones actualizadas", n)
	w.Header().Set("HX-Redirect", "/sources")
}

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
		defer safe.Recover("prueba de source")
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
