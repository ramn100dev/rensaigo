// Package api: servidor HTTP, UI renderizada en servidor (html/template + htmx), lector y API de estado.
package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"rensaigo/internal/config"
	"rensaigo/internal/extrepo"
	"rensaigo/internal/hostproc"
	"rensaigo/internal/jobs"
	"rensaigo/internal/jsengine"
	"rensaigo/internal/library"
	"rensaigo/internal/store"
	"rensaigo/internal/track"
	"rensaigo/web"
)

type Server struct {
	Cfg    *config.Config
	St     *store.Store
	Host   *hostproc.Supervisor
	Repo   *extrepo.Repo
	Eng    *jobs.Engine
	Thumbs *library.Thumbs
	tpl    *template.Template
	token  string
	start  time.Time

	scanMu   sync.Mutex
	lastScan time.Time

	Trk        []track.Service // por orden de prioridad para los metadatos
	importSlot chan struct{}   // búsquedas/importaciones desde listas en curso (como mucho 2)
	trkMu      sync.Mutex      // subidas de progreso a los servicios, de una en una

	probeMu sync.Mutex
	probing map[string]bool // sources que se están probando en segundo plano

	alertC alertCache // avisos fijos de la campana
	cov    covState   // medición de cobertura de las extensiones
	ntf    notifier   // capítulos pendientes de avisar fuera (Telegram, ntfy…)
	dirty  dirtySet   // series con datos por guardar en su carpeta
}

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
	if rep.SeriesNew > 0 || rep.ChaptersNew > 0 || rep.MissingOnDisk > 0 {
		s.St.Event("biblioteca", "escaneo: %d series y %d capítulos nuevos adoptados del disco, %d desaparecidos (%d ms)",
			rep.SeriesNew, rep.ChaptersNew, rep.MissingOnDisk, rep.DurationMillis)
	}
	return rep
}

type M = map[string]any

var stateLabel = map[string]string{
	"available": "disponible", "queued": "en cola", "downloading": "descargando", "downloaded": "descargado", "failed": "fallido",
	"ok": "ok", "cloudflare": "Cloudflare", "webview": "requiere navegador", "javascript": "requiere JavaScript", "android": "API de Android no soportada",
	"http": "error HTTP", "network": "error de red", "extension": "error de extensión", "unknown": "sin probar",
}

func New(cfg *config.Config, st *store.Store, host *hostproc.Supervisor, repo *extrepo.Repo, eng *jobs.Engine, th *library.Thumbs) (*Server, error) {
	s := &Server{Cfg: cfg, St: st, Host: host, Repo: repo, Eng: eng, Thumbs: th, start: time.Now(),
		importSlot: make(chan struct{}, 2)}
	s.covInit()
	s.ApplyAdult()
	s.Trk = []track.Service{track.NewMAL(st), track.NewAniList(st), track.NewMangaDex(st, func() string { return s.Cfg.PrefLang })}
	// la sesión del navegador sobrevive a los reinicios: el token se guarda (si no, cada actualización echaba fuera)
	if s.token = st.Get("auth.token", ""); len(s.token) < 32 {
		b := make([]byte, 24)
		rand.Read(b)
		s.token = hex.EncodeToString(b)
		st.Set("auth.token", s.token)
	}
	funcs := template.FuncMap{
		"dict": func(kv ...any) M {
			m := M{}
			for i := 0; i+1 < len(kv); i += 2 {
				m[kv[i].(string)] = kv[i+1]
			}
			return m
		},
		"label": func(s string) string {
			if l, ok := stateLabel[s]; ok {
				return l
			}
			return s
		},
		"num": func(v any) string { return strconv.FormatFloat(toF(v), 'f', -1, 64) },
		"ago": func(v any) string {
			ts := int64(toF(v))
			if ts == 0 {
				return "nunca"
			}
			d := time.Since(time.Unix(ts, 0)).Round(time.Second)
			switch {
			case d < time.Minute:
				return "hace " + d.String()
			case d < time.Hour:
				return fmt.Sprintf("hace %d min", int(d.Minutes()))
			case d < 48*time.Hour:
				return fmt.Sprintf("hace %d h", int(d.Hours()))
			}
			return fmt.Sprintf("hace %d días", int(d.Hours()/24))
		},
		"mb":     func(v any) string { return fmt.Sprintf("%.1f MB", toF(v)/1e6) },
		"kmb":    func(v any) string { return fmt.Sprintf("%.1f MB", toF(v)/1024) },
		"pct100": func(v any) int { return int(toF(v)*100 + 0.5) },
		"pct": func(a, b any) int {
			if toF(b) == 0 {
				return 0
			}
			return int(toF(a) * 100 / toF(b))
		},
		"add":  func(a, b any) int { return int(toF(a) + toF(b)) },
		"icon": icon,
		"v":    func() string { return strconv.FormatInt(s.start.Unix(), 36) }, // invalida la caché de los estáticos en cada arranque
		"dur": func(v any) string {
			d := time.Duration(toF(v)) * time.Second
			switch {
			case d < time.Hour:
				return fmt.Sprintf("%d min", int(d.Minutes()))
			case d < 48*time.Hour:
				return fmt.Sprintf("%d h", int(d.Hours()))
			}
			return fmt.Sprintf("%d días", int(d.Hours()/24))
		},
	}
	var err error
	s.tpl, err = template.New("").Funcs(funcs).ParseFS(web.FS, "templates/*.html")
	return s, err
}

// toF convierte cualquier número de plantilla (int, int64, uint64, float64…) a float64.
func toF(v any) float64 {
	switch n := v.(type) {
	case int:
		return float64(n)
	case int32:
		return float64(n)
	case int64:
		return float64(n)
	case uint64:
		return float64(n)
	case float64:
		return n
	case float32:
		return float64(n)
	}
	return 0
}

func (s *Server) render(w http.ResponseWriter, name string, data M) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("plantilla %s: %v", name, err)
	}
}

func (s *Server) page(w http.ResponseWriter, tab, body string, data M) {
	if data == nil {
		data = M{}
	}
	data["Tab"], data["Body"], data["Auth"] = tab, body, s.Cfg.Password != ""
	s.render(w, "layout", data)
}

func fail(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	fmt.Fprintf(w, `<div class="err">%s</div>`, template.HTMLEscapeString(friendlyErr(err)))
}

func pathInt(r *http.Request, name string) int64 {
	n, _ := strconv.ParseInt(r.PathValue(name), 10, 64)
	return n
}

// rows ejecuta una consulta y devuelve cada fila como mapa columna->valor.
func (s *Server) rows(q string, args ...any) []M {
	rs, err := s.St.DB.Query(q, args...)
	if err != nil {
		log.Printf("sql: %v", err)
		return nil
	}
	defer rs.Close()
	cols, _ := rs.Columns()
	var out []M
	for rs.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		rs.Scan(ptrs...)
		m := M{}
		for i, c := range cols {
			if b, ok := vals[i].([]byte); ok {
				m[c] = string(b)
			} else {
				m[c] = vals[i]
			}
		}
		out = append(out, m)
	}
	return out
}

func (s *Server) row(q string, args ...any) M {
	r := s.rows(q, args...)
	if len(r) == 0 {
		return nil
	}
	return r[0]
}

// ---------------------------------------------------------------- auth

func (s *Server) auth(next http.Handler) http.Handler {
	if s.Cfg.Password == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /opds/ lleva su propia clave en la ruta (las apps de lectura no manejan cookies)
		if r.URL.Path == "/login" || strings.HasPrefix(r.URL.Path, "/static/") || strings.HasPrefix(r.URL.Path, "/opds/") {
			next.ServeHTTP(w, r)
			return
		}
		if c, err := r.Cookie("rensaigo"); err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.session())) == 1 {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") || r.Header.Get("HX-Request") != "" {
			w.Header().Set("HX-Redirect", "/login")
			http.Error(w, "no autenticado", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
}

// session: el valor de la cookie. Depende de la contraseña: al cambiarla, las sesiones abiertas dejan de valer.
func (s *Server) session() string {
	h := sha256.Sum256([]byte(s.token + "\x00" + s.Cfg.Password))
	return hex.EncodeToString(h[:])
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		time.Sleep(300 * time.Millisecond) // freno simple a la fuerza bruta
		if subtle.ConstantTimeCompare([]byte(r.FormValue("password")), []byte(s.Cfg.Password)) == 1 {
			http.SetCookie(w, &http.Cookie{Name: "rensaigo", Value: s.session(), Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 30 * 86400})
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		s.render(w, "login", M{"Error": "Contraseña incorrecta"})
		return
	}
	s.render(w, "login", M{})
}

// ---------------------------------------------------------------- rutas

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(web.FS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheStatic(http.FileServerFS(static))))
	mux.HandleFunc("/login", s.login)
	mux.HandleFunc("POST /logout", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "rensaigo", Value: "", Path: "/", MaxAge: -1})
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/library", http.StatusSeeOther) })

	mux.HandleFunc("GET /library", s.libraryIndex)
	mux.HandleFunc("GET /library/{id}", s.librarySeries)
	mux.HandleFunc("GET /library/grid", s.libraryGrid)
	mux.HandleFunc("GET /read/{id}", s.reader)
	mux.HandleFunc("GET /page/{id}/{n}", s.pageImage)
	mux.HandleFunc("GET /thumb/{id}", s.thumb)
	mux.HandleFunc("POST /api/progress", s.progress)

	mux.HandleFunc("GET /series", s.seriesIndex)
	mux.HandleFunc("GET /series/search", s.seriesSearch)
	mux.HandleFunc("POST /series/follow", s.seriesFollow)
	mux.HandleFunc("GET /series/{id}", s.seriesDetail)
	mux.HandleFunc("GET /series/{id}/chapters", s.seriesChapters)
	mux.HandleFunc("GET /series/{id}/candidates", s.seriesCandidates)
	mux.HandleFunc("GET /series/{id}/candstat", s.candStat)
	mux.HandleFunc("GET /series/{id}/sswidth", s.ssWidth)
	mux.HandleFunc("POST /series/{id}/{action}", s.seriesAction)
	mux.HandleFunc("POST /chapter/{id}/retry", s.chapterRetry)
	mux.HandleFunc("POST /chapter/{id}/read", s.chapterRead)
	mux.HandleFunc("GET /queue", s.queue)
	mux.HandleFunc("GET /hdr/pill", s.hdrPill)
	mux.HandleFunc("GET /hdr/queue", s.hdrQueue)
	mux.HandleFunc("GET /hdr/events", s.hdrEvents)
	s.opdsRoutes(mux)
	mux.HandleFunc("POST /alerts/{act}", s.alertAction)
	mux.HandleFunc("GET /notify", s.notifyPanel)
	mux.HandleFunc("POST /notify/{act}", s.notifyAction)
	mux.HandleFunc("GET /icon/{pkg}", s.extIcon)
	mux.HandleFunc("POST /settings", s.settingsSave)
	s.trackRoutes(mux)

	mux.HandleFunc("GET /sources", s.sourcesIndex)
	mux.HandleFunc("GET /sources/repo", s.sourcesRepo)
	mux.HandleFunc("POST /sources/refresh", s.sourcesRefresh)
	mux.HandleFunc("POST /sources/install", s.sourcesInstall)
	mux.HandleFunc("POST /sources/coverage", s.covStart)
	mux.HandleFunc("POST /sources/coverage/stop", s.covStop)
	mux.HandleFunc("GET /sources/coverage", s.covStatus)
	mux.HandleFunc("POST /sources/uninstall", s.sourcesUninstall)
	mux.HandleFunc("POST /sources/{id}/toggle", s.sourcesToggle)
	mux.HandleFunc("POST /sources/{id}/test", s.sourcesTest)
	mux.HandleFunc("GET /sources/{id}/prefs", s.sourcePrefs)
	mux.HandleFunc("POST /sources/{id}/prefs", s.sourcePrefSet)
	mux.HandleFunc("POST /sources/{id}/version", s.sourceVersion)
	mux.HandleFunc("POST /downloads/pause", s.downloadsPause)
	mux.HandleFunc("POST /sources/update-all", s.sourcesUpdateAll)

	mux.HandleFunc("GET /import", s.importIndex)
	mux.HandleFunc("POST /import/scan", s.importScan)
	mux.HandleFunc("GET /import/{id}/candidates", s.importCandidates)
	mux.HandleFunc("POST /import/{id}/match", s.importMatch)
	mux.HandleFunc("POST /import/{id}/auto", s.importAuto)

	mux.HandleFunc("GET /system", s.system)
	mux.HandleFunc("POST /system/{action}", s.systemAction)
	mux.HandleFunc("GET /api/status", s.apiStatus)
	// API JSON mínima (la usan las pruebas de test/ y sirve de base para la v1)
	mux.HandleFunc("GET /api/sources", func(w http.ResponseWriter, r *http.Request) {
		s.json(w, s.rows(`SELECT s.id, s.pkg, s.name, s.lang, s.base_url, s.enabled, s.status, s.status_msg, s.status_at FROM source s ORDER BY s.name`))
	})
	mux.HandleFunc("GET /api/series", func(w http.ResponseWriter, r *http.Request) { s.json(w, s.seriesList()) })
	mux.HandleFunc("GET /api/chapters", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.FormValue("series"), 10, 64)
		s.json(w, s.rows(`SELECT id, number, name, state, pages, bytes, attempts, error, cbz, series_source_id FROM chapter WHERE series_id=? ORDER BY number`, id))
	})
	mux.HandleFunc("GET /api/search", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		ms, err := s.Eng.Search(ctx, r.FormValue("source"), r.FormValue("q"))
		if err != nil {
			w.WriteHeader(502)
			s.json(w, M{"error": err.Error(), "kind": hostproc.Kind(err)})
			return
		}
		s.json(w, ms)
	})
	mux.HandleFunc("GET /api/repo", func(w http.ResponseWriter, r *http.Request) {
		s.json(w, s.rows(`SELECT pkg, name, lang, version, nsfw FROM repo_ext ORDER BY pkg`))
	})
	mux.HandleFunc("POST /api/install", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
		defer cancel()
		srcs, err := s.Repo.Install(ctx, r.FormValue("pkg"))
		if err != nil {
			w.WriteHeader(502)
			s.json(w, M{"error": err.Error(), "kind": hostproc.Kind(err)})
			return
		}
		s.json(w, srcs)
	})
	mux.HandleFunc("POST /api/probe", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 4*time.Minute)
		defer cancel()
		s.json(w, s.Eng.Probe(ctx, r.FormValue("source")))
	})
	mux.HandleFunc("GET /api/events", func(w http.ResponseWriter, r *http.Request) {
		s.json(w, s.rows(`SELECT at, kind, msg FROM event ORDER BY id DESC LIMIT 100`))
	})
	return s.auth(mux)
}

func cacheStatic(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		h.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------- biblioteca y lector

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
	srcs := s.rows(`SELECT ss.id, ss.priority, ss.url, ss.title, ss.last_check, ss.last_error, ss.temporary, ss.from_ch, src.name, src.lang, src.status, src.enabled, src.pkg
		FROM series_source ss LEFT JOIN source src ON src.id=ss.source_id WHERE ss.series_id=? ORDER BY ss.priority`, id)
	for _, x := range srcs {
		x["width"] = s.ssWidthDisk(x["id"].(int64))
	}
	s.page(w, "library", "library_series", M{"S": se, "Chapters": s.chapterList(id), "Next": next, "SeriesSources": srcs, "Sources": s.enabledSources(), "Trk": s.trkBoxes(id, nil)})
}

func (s *Server) reader(w http.ResponseWriter, r *http.Request) {
	id := pathInt(r, "id")
	c := s.row(`SELECT c.id, c.series_id, c.number, c.name, c.pages, s.title, coalesce(p.page,0) AS page, coalesce(p.done,0) AS done
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

const placeholder = `<svg xmlns="http://www.w3.org/2000/svg" width="300" height="430"><rect width="100%" height="100%" fill="#262a33"/><text x="50%" y="50%" fill="#778" font-family="sans-serif" font-size="20" text-anchor="middle">sin portada</text></svg>`

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
	io.WriteString(w, placeholder)
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
		go s.syncSeries(sid, false)
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------- series y descargas

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
		libChanged(w, "«"+m.Title+"» añadida a la biblioteca")
		s.render(w, "follow_done", M{"ID": id, "Title": m.Title, "Thumbnail": m.Thumbnail})
		return
	}
	redirect(w, r, fmt.Sprintf("/library/%d", id))
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
		go s.syncSeries(sid, false)
	} else {
		s.St.DB.Exec(`DELETE FROM progress WHERE chapter_id=?`, id)
		go s.syncSeries(sid, true) // desmarcar a mano sí baja el contador del servicio
	}
	s.seriesChaptersFor(w, sid)
}

// redirect manda a otra página. A htmx solo con HX-Redirect: si además se responde 303, el navegador sigue la
// redirección por su cuenta, htmx nunca ve la cabecera y acaba incrustando la página entera en el destino del swap.
func redirect(w http.ResponseWriter, r *http.Request, url string) {
	if r.Header.Get("HX-Request") != "" {
		w.Header().Set("HX-Redirect", url)
		return
	}
	http.Redirect(w, r, url, http.StatusSeeOther)
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
		toast = "Rastreo pausado"
	case "resume":
		_, err = s.St.DB.Exec(`UPDATE series SET paused=0 WHERE id=?`, id)
		s.Eng.Wake()
		toast = "Rastreo reanudado"
	case "fav":
		_, err = s.St.DB.Exec(`UPDATE series SET favorite=1-favorite WHERE id=?`, id)
	case "verify":
		var ok, bad int
		ok, bad, err = s.verifySeries(id)
		if bad > 0 {
			toast = fmt.Sprintf("%d capítulos correctos · %d ilegibles o desaparecidos, vuelven a «disponible»", ok, bad)
		} else {
			toast = fmt.Sprintf("Los %d capítulos del disco están bien", ok)
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
		toast = "Source consultado"
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
			go s.Eng.CheckSeries(context.Background(), id)
		}
	case "unsource":
		ssid, _ := strconv.ParseInt(r.FormValue("ss"), 10, 64)
		s.St.DB.Exec(`UPDATE chapter SET series_source_id=NULL, upg=0 WHERE series_source_id=? AND state='downloaded'`, ssid)
		s.St.DB.Exec(`DELETE FROM chapter WHERE series_source_id=? AND state<>'downloaded'`, ssid)
		if _, err = s.St.DB.Exec(`DELETE FROM series_source WHERE id=? AND series_id=?`, ssid, id); err == nil {
			s.Eng.NormSources(id)
			go s.Eng.CheckSeries(context.Background(), id) // lo pendiente que tenía puede estar en otro
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
		go s.Eng.CheckSeries(context.Background(), id)
	case "delete": // quita la serie Y BORRA su carpeta de la biblioteca
		if err = s.deleteSeries(id); err == nil {
			if card {
				setToast(w, "Serie eliminada junto con sus archivos")
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

func (s *Server) queue(w http.ResponseWriter, r *http.Request) {
	snap := s.Eng.Snapshot()
	s.render(w, "queue", M{"Snap": snap, "Host": s.Host.Stats()})
}

// ---------------------------------------------------------------- sources

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
	kept := srcs[:0]
	for _, src := range srcs {
		if src["pkg"] == trial {
			continue // instalada solo para medirla: no se muestra
		}
		if _, ok := adult[src["id"].(string)]; ok {
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
	s.ApplyAdult()
	adult := s.adultSiblings()
	for _, in := range infos {
		if _, ok := adult[in.ID]; !ok {
			s.probeSource(in.ID) // se prueba sola: no hace falta pulsar «probar»
		}
	}
	w.Header().Set("HX-Redirect", "/sources")
}

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
		setToast(w, name+": no responde · "+trunc(err.Error(), 120))
	} else {
		setToast(w, name+": funciona")
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
	s.render(w, "source_prefs", M{"ID": id, "Prefs": pr.Prefs, "Saved": r.FormValue("saved"), "PrefErr": prefErr, "Adult": adult,
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
		setToast(w, "Ahora usa la versión "+v+" (fijada)")
		w.Header().Set("HX-Trigger-After-Settle", `{"srcchanged":true}`)
	} else {
		s.Repo.SetPinned(pkg, pin)
		if pin {
			setToast(w, "Fijada: no se actualizará sola")
		} else {
			setToast(w, "Se actualizará con las demás")
		}
		w.Header().Set("HX-Trigger-After-Settle", `{"srcchanged":true}`)
	}
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

// ---------------------------------------------------------------- importación

// importData: datos de la sección "Importar biblioteca", que ahora forma parte de Sistema.
func (s *Server) importData() M {
	pending := s.rows(`SELECT s.id, s.title, s.category, s.hint_source,
		(SELECT count(*) FROM chapter WHERE series_id=s.id AND state='downloaded') AS chapters,
		'all' AS suggested
		FROM series s WHERE s.imported=1 AND NOT EXISTS(SELECT 1 FROM series_source WHERE series_id=s.id) ORDER BY s.title`)
	var rep *library.Report
	if j := s.St.Get("last_import", ""); j != "" {
		rep = &library.Report{}
		json.Unmarshal([]byte(j), rep)
	}
	return M{"Pending": pending, "Report": rep, "Sources": s.enabledSources(), "Lib": s.Cfg.LibraryDir}
}

func (s *Server) importIndex(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/system#importar", http.StatusSeeOther)
}

func (s *Server) importScan(w http.ResponseWriter, r *http.Request) {
	rep := s.QuickScan(true)
	if rep == nil {
		fail(w, 500, fmt.Errorf("no se pudo escanear %s", s.Cfg.LibraryDir))
		return
	}
	w.Header().Set("HX-Redirect", "/system#importar")
	if r.Header.Get("HX-Request") == "" {
		json.NewEncoder(w).Encode(rep)
	}
}

func (s *Server) importCandidates(w http.ResponseWriter, r *http.Request) {
	id := pathInt(r, "id")
	q := r.FormValue("q")
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	ms, err := s.Eng.Search(ctx, r.FormValue("source"), q)
	if len(ms) > 12 {
		ms = ms[:12]
	}
	data := M{"ID": id, "Source": r.FormValue("source"), "Mangas": ms, "Q": q, "Sources": s.enabledSources()}
	if err != nil {
		data["Err"] = friendlyErr(err)
	}
	tpl := "import_modal"
	if r.FormValue("partial") == "1" {
		tpl = "import_candidates"
	}
	s.render(w, tpl, data)
}

// importMatch: el usuario confirma qué serie del source corresponde. Se enlaza y se comprueba; los capítulos
// ya presentes en disco se reconocen por número y NO se vuelven a descargar.
func (s *Server) importMatch(w http.ResponseWriter, r *http.Request) {
	id := pathInt(r, "id")
	if err := s.Eng.AddSource(id, r.FormValue("source"), r.FormValue("url"), r.FormValue("title"), r.FormValue("memo")); err != nil {
		fail(w, 500, err)
		return
	}
	s.St.DB.Exec(`UPDATE series SET paused=0, auto_download=? WHERE id=?`, b2i(r.FormValue("auto") == "1"), id)
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	if err := s.Eng.CheckSeries(ctx, id); err != nil {
		fail(w, 502, err)
		return
	}
	w.Header().Set("HX-Redirect", fmt.Sprintf("/library/%d", id))
}

// importAuto empareja una serie importada sin abrir diálogos:
//   - con un source concreto busca el título y, si hay un resultado claramente igual, lo enlaza;
//   - con «all» busca en todos los sources activos y devuelve la fila con los candidatos para que el usuario elija;
//   - con url (candidato elegido) lo enlaza directamente.
//
// Al enlazar se comprueba la serie y se deja la descarga automática activada; la fila desaparece de la tabla.
func (s *Server) importAuto(w http.ResponseWriter, r *http.Request) {
	id := pathInt(r, "id")
	src := r.FormValue("source")
	var title string
	s.St.DB.QueryRow(`SELECT title FROM series WHERE id=?`, id).Scan(&title)
	srcName := func(id string) (n string) {
		s.St.DB.QueryRow(`SELECT name FROM source WHERE id=?`, id).Scan(&n)
		return
	}
	row := func(extra M) {
		d := s.importData()
		for _, p := range d["Pending"].([]M) {
			if p["id"].(int64) == id {
				p["suggested"] = src
				for k, v := range extra {
					p[k] = v
				}
				s.render(w, "pair_row", M{"P": p, "Sources": d["Sources"]})
				return
			}
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Minute)
	defer cancel()
	link := func(src, url, mtitle, memo string) {
		if err := s.Eng.AddSource(id, src, url, mtitle, memo); err != nil {
			row(M{"msg": err.Error()})
			return
		}
		s.St.DB.Exec(`UPDATE series SET paused=0, auto_download=1 WHERE id=?`, id)
		if err := s.Eng.CheckSeries(ctx, id); err != nil {
			s.St.Event("importación", "«%s» enlazada con %s, pero la comprobación falló: %v", title, srcName(src), err)
		}
		setToast(w, fmt.Sprintf("«%s» emparejada con %s", title, srcName(src)))
	}

	if url := r.FormValue("url"); url != "" {
		link(src, url, r.FormValue("title"), r.FormValue("memo"))
		return
	}

	if src == "all" {
		cands, fails := s.searchAll(ctx, []string{title})
		if len(cands) > 12 {
			cands = cands[:12]
		}
		extra := M{"cands": cands}
		if len(cands) == 0 {
			extra["msg"] = "No aparece en ningún source activo"
		}
		if len(fails) > 0 {
			extra["fails"] = strings.Join(fails, ", ")
		}
		row(extra)
		return
	}

	ms, err := s.Eng.Search(ctx, src, title)
	if err != nil {
		row(M{"msg": friendlyErr(err)})
		return
	}
	best, score := -1, 0.0
	for i, m := range ms {
		if sc := titleScore(title, m.Title); sc > score {
			best, score = i, sc
		}
	}
	if best < 0 || score < 0.8 {
		msg := "No aparece en " + srcName(src)
		if best >= 0 {
			msg += " (lo más parecido: «" + ms[best].Title + "»)"
		}
		row(M{"msg": msg})
		return
	}
	m := ms[best]
	link(src, m.URL, m.Title, string(m.Memo))
}

// titleScore compara dos títulos sin mayúsculas, tildes ni signos: 1 si son iguales, si no el coeficiente de Dice
// de sus bigramas.
func titleScore(a, b string) float64 {
	a, b = normTitle(a), normTitle(b)
	if a == "" || b == "" {
		return 0
	}
	if a == b {
		return 1
	}
	bg := func(x string) map[string]int {
		m := map[string]int{}
		rs := []rune(x)
		for i := 0; i+1 < len(rs); i++ {
			m[string(rs[i:i+2])]++
		}
		return m
	}
	ma, mb := bg(a), bg(b)
	var inter, n int
	for k, v := range ma {
		n += v
		if w := mb[k]; w > 0 {
			inter += min(v, w)
		}
	}
	for _, v := range mb {
		n += v
	}
	if n == 0 {
		return 0
	}
	return 2 * float64(inter) / float64(n)
}

func normTitle(t string) string {
	rep := strings.NewReplacer("á", "a", "é", "e", "í", "i", "ó", "o", "ú", "u", "ü", "u", "ñ", "n", "à", "a", "è", "e", "ò", "o")
	t = rep.Replace(strings.ToLower(t))
	var b strings.Builder
	for _, r := range t {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r > 127 {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---------------------------------------------------------------- sistema

type Status struct {
	UptimeS    int64          `json:"uptime_s"`
	CoreRSSKb  int64          `json:"core_rss_kb"`
	CoreHWMKb  int64          `json:"core_hwm_kb"`
	GoHeapKb   uint64         `json:"go_heap_kb"`
	GoSysKb    uint64         `json:"go_sys_kb"`
	Goroutines int            `json:"goroutines"`
	Host       hostproc.Stats `json:"host"`
	Queue      jobs.Snapshot  `json:"queue"`
	ThumbBytes int64          `json:"thumb_cache_bytes"`
	ThumbFiles int            `json:"thumb_cache_files"`
	DBBytes    int64          `json:"db_bytes"`
	Series     int            `json:"series"`
	Chapters   int            `json:"chapters"`
}

func selfMem() (rss, hwm int64) {
	b, _ := os.ReadFile("/proc/self/status")
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) >= 2 {
			switch f[0] {
			case "VmRSS:":
				rss, _ = strconv.ParseInt(f[1], 10, 64)
			case "VmHWM:":
				hwm, _ = strconv.ParseInt(f[1], 10, 64)
			}
		}
	}
	return
}

func (s *Server) status() Status {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	st := Status{UptimeS: int64(time.Since(s.start).Seconds()), GoHeapKb: ms.HeapAlloc / 1024, GoSysKb: ms.Sys / 1024,
		Goroutines: runtime.NumGoroutine(), Host: s.Host.Stats(), Queue: s.Eng.Snapshot()}
	st.CoreRSSKb, st.CoreHWMKb = selfMem()
	st.ThumbBytes, st.ThumbFiles = s.Thumbs.Size()
	for _, suf := range []string{"", "-wal"} {
		if fi, err := os.Stat(s.Cfg.Path("rensaigo.db") + suf); err == nil {
			st.DBBytes += fi.Size()
		}
	}
	s.St.DB.QueryRow(`SELECT count(*) FROM series`).Scan(&st.Series)
	s.St.DB.QueryRow(`SELECT count(*) FROM chapter`).Scan(&st.Chapters)
	return st
}

func (s *Server) json(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if rows, ok := v.([]M); ok && rows == nil {
		v = []M{}
	}
	json.NewEncoder(w).Encode(v)
}

func (s *Server) apiStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.status())
}

func (s *Server) system(w http.ResponseWriter, r *http.Request) {
	backups, _ := filepath.Glob(s.Cfg.Path("backup", "*.db"))
	var bk []M
	for _, b := range backups {
		if fi, err := os.Stat(b); err == nil {
			bk = append(bk, M{"name": filepath.Base(b), "size": fi.Size(), "at": fi.ModTime().Unix()})
		}
	}
	var last int64
	for _, b := range bk {
		if t := b["at"].(int64); t > last {
			last = t
		}
	}
	s.page(w, "system", "system", M{"St": s.status(), "Groups": s.settingGroups(), "Conns": s.connectedCount(),
		"Backups": bk, "LastBackup": last, "Cfg": s.Cfg, "Flare": s.Cfg.FlareSolverr != "", "Import": s.importData(),
		"JS": s.js().Installed(), "JSBytes": s.js().Size(),
		"JSNeed": s.rows(`SELECT name FROM source WHERE status='javascript' ORDER BY name`)})
}

func (s *Server) js() *jsengine.Engine { return &jsengine.Engine{Dir: s.Cfg.Path("jsengine")} }

func (s *Server) systemAction(w http.ResponseWriter, r *http.Request) {
	switch r.PathValue("action") {
	case "backup":
		if _, err := s.St.Backup(s.Cfg.Path("backup"), s.Cfg.BackupKeep); err != nil {
			fail(w, 500, err)
			return
		}
	case "host-stop":
		s.Host.Stop()
	case "js-install":
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
		defer cancel()
		if err := s.js().Install(ctx); err != nil {
			fail(w, 502, err)
			return
		}
		s.Host.Stop() // el siguiente arranque ya lleva el motor
		s.St.Event("javascript", "motor JavaScript (GraalJS) instalado y verificado: %.0f MB", float64(s.js().Size())/1e6)
	case "js-remove":
		s.js().Remove()
		s.Host.Stop()
	case "check-all":
		s.Eng.CheckAll()
	case "reset-peak":
		s.Host.ResetPeak()
	default:
		http.NotFound(w, r)
		return
	}
	w.Header().Set("HX-Redirect", "/system")
}

var _ = sql.ErrNoRows
