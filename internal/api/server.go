// Package api: servidor HTTP, UI renderizada en servidor (html/template + htmx), lector y API de estado.
//
// Un archivo por pantalla o tema: library (biblioteca y ficha de serie), reader (lector, páginas y portadas),
// series (buscar y seguir), sources (extensiones), import (adoptar una biblioteca existente), system (ajustes,
// estado y cabecera), track* (MyAnimeList, AniList, MangaDex), opds, notify, alerts, coverage. Aquí, el
// servidor y lo que comparten todos.
package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"rensaigo/internal/config"
	"rensaigo/internal/extrepo"
	"rensaigo/internal/hostproc"
	"rensaigo/internal/i18n"
	"rensaigo/internal/jobs"
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
	offline  bool // el último escaneo no encontró la biblioteca (disco desmontado)

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

type M = map[string]any

var stateLabel = map[string]string{
	"available": "disponible", "queued": "en cola", "downloading": "descargando", "downloaded": "descargado", "failed": "fallido",
	"ok": "ok", "cloudflare": "Cloudflare", "webview": "requiere navegador", "javascript": "requiere JavaScript", "android": "API de Android no soportada",
	"http": "error HTTP", "missing": "extensión no instalada", "network": "error de red", "extension": "error de extensión", "unknown": "sin probar",
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
				return i18n.T(l)
			}
			return s
		},
		"t":  i18n.T,
		"tf": func(f string, a ...any) string { return i18n.Tf(f, intArgs(a)...) },
		"tn": func(n any, one, many string) string {
			if v := intArgs([]any{n})[0]; v != nil {
				n = v
			}
			return i18n.Tn(n, one, many)
		},
		"lang":     i18n.Lang,
		"jsdict":   jsDict,
		"optlabel": optLabel,
		"num":      func(v any) string { return strconv.FormatFloat(toF(v), 'f', -1, 64) },
		"ago": func(v any) string {
			ts := int64(toF(v))
			if ts == 0 {
				return i18n.T("nunca")
			}
			d := time.Since(time.Unix(ts, 0)).Round(time.Second)
			switch {
			case d < time.Minute:
				return i18n.Tf("hace %d s", int(d.Seconds()))
			case d < time.Hour:
				return i18n.Tf("hace %d min", int(d.Minutes()))
			case d < 48*time.Hour:
				return i18n.Tf("hace %d h", int(d.Hours()))
			}
			return i18n.Tf("hace %d días", int(d.Hours()/24))
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
			return i18n.Tf("%d días", int(d.Hours()/24))
		},
	}
	var err error
	s.tpl, err = template.New("").Funcs(funcs).ParseFS(web.FS, "templates/*.html")
	return s, err
}

// intArgs: los números enteros que llegan como float64 (de la BD o de JSON) pasan a int64, para que %d funcione.
func intArgs(a []any) []any {
	out := make([]any, len(a))
	for i, v := range a {
		if f, ok := v.(float64); ok && f == float64(int64(f)) {
			v = int64(f)
		}
		out[i] = v
	}
	return out
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

// redirect manda a otra página. A htmx solo con HX-Redirect: si además se responde 303, el navegador sigue la
// redirección por su cuenta, htmx nunca ve la cabecera y acaba incrustando la página entera en el destino del swap.
func redirect(w http.ResponseWriter, r *http.Request, url string) {
	if r.Header.Get("HX-Request") != "" {
		w.Header().Set("HX-Redirect", url)
		return
	}
	http.Redirect(w, r, url, http.StatusSeeOther)
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s *Server) json(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if rows, ok := v.([]M); ok && rows == nil {
		v = []M{}
	}
	json.NewEncoder(w).Encode(v)
}

// icon pinta un icono del sprite que va en el layout (#i-<nombre>).
func icon(name string) template.HTML {
	return template.HTML(`<svg class="i" aria-hidden="true"><use href="#i-` + template.HTMLEscapeString(name) + `"/></svg>`)
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
