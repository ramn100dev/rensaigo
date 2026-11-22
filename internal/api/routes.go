package api

import (
	"context"
	"io/fs"
	"net/http"
	"strconv"
	"time"

	"rensaigo/internal/hostproc"
	"rensaigo/web"
)

// Rutas de la web y de la API JSON mínima.

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
	mux.HandleFunc("POST /sources/{id}/lang", s.sourceLang)
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
	// formularios enviados desde otra web (CSRF): sin contraseña, cualquier página que visites podría, p. ej., borrar
	// una serie con un POST a esta dirección. Las peticiones de otro origen que cambian algo se rechazan.
	return http.NewCrossOriginProtection().Handler(s.auth(mux))
}

func cacheStatic(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		h.ServeHTTP(w, r)
	})
}
