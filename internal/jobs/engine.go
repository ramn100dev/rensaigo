// Package jobs: llamadas a sources (con rate-limit y estado), seguimiento de series, planificador y cola de descargas.
//
// engine.go: el motor y las llamadas al host; series.go: seguir series y comprobar capítulos; queue.go: la cola
// de descargas; probe.go: pruebas y mediciones de un source.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"rensaigo/internal/config"
	"rensaigo/internal/hostproc"
	"rensaigo/internal/store"
)

// Host ejecuta una operación de una extensión (el host JVM; en los tests, uno falso).
type Host interface {
	CallJar(ctx context.Context, jar, op string, args map[string]any) (json.RawMessage, error)
}

// Jars dice qué jar (ya verificado) tiene cada source.
type Jars interface {
	JarFor(sourceID string) (string, error)
}

type Engine struct {
	Cfg  *config.Config
	St   *store.Store
	Host Host
	Repo Jars

	wake chan struct{}
	mu   sync.Mutex
	// descargas activas: capítulo -> progreso
	active  map[int64]*Active
	srcBusy map[string]int
	lim     map[string]*limiter
	served  map[int64]int64 // serie → turno en que empezó su última descarga (para repartir por turnos)
	turn    int64
	checkMu sync.Map // serie → *sync.Mutex: una comprobación a la vez por serie (series distintas van en paralelo)
	Checks  int64    // comprobaciones de planificador completadas

	OnChange func(seriesID int64) // algo de la serie cambió (comprobación, descarga): para guardar sus datos en disco
	// OnDownloaded: capítulo nuevo en disco (no las sustituciones de uno temporal). Para las notificaciones
	OnDownloaded func(seriesID int64, series string, number float64)
}

func (e *Engine) changed(seriesID int64) {
	if e.OnChange != nil {
		e.OnChange(seriesID)
	}
}

type Active struct {
	ChapterID int64  `json:"chapter_id"`
	Series    string `json:"series"`
	Chapter   string `json:"chapter"`
	Source    string `json:"source"`
	Done      int    `json:"done"`
	Total     int    `json:"total"`

	series int64
	cancel context.CancelFunc
}

// errGone: el capítulo dejó de existir mientras se descargaba (serie borrada, source quitado): no se guarda nada.
var errGone = errors.New("capítulo eliminado durante la descarga")

// CancelSeries para las descargas en curso de una serie (se va a borrar) y espera un poco a que terminen, para
// que ninguna vuelva a crear su carpeta.
func (e *Engine) CancelSeries(seriesID int64) {
	for i := 0; i < 50; i++ {
		n := 0
		e.mu.Lock()
		for _, a := range e.active {
			if a.series == seriesID {
				a.cancel()
				n++
			}
		}
		e.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

type limiter struct {
	mu   sync.Mutex
	next time.Time
}

func New(cfg *config.Config, st *store.Store, host Host, repo Jars) *Engine {
	return &Engine{Cfg: cfg, St: st, Host: host, Repo: repo, wake: make(chan struct{}, 1),
		active: map[int64]*Active{}, srcBusy: map[string]int{}, lim: map[string]*limiter{}}
}

func (e *Engine) Wake() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// wait aplica la separación mínima entre peticiones a un mismo source.
func (e *Engine) wait(ctx context.Context, sourceID string) error {
	e.mu.Lock()
	l := e.lim[sourceID]
	if l == nil {
		l = &limiter{}
		e.lim[sourceID] = l
	}
	e.mu.Unlock()
	l.mu.Lock()
	now := time.Now()
	at := l.next
	if at.Before(now) {
		at = now
	}
	l.next = at.Add(e.Cfg.RateInterval)
	l.mu.Unlock()
	if d := time.Until(at); d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// SourceCall ejecuta una operación de un source a través del host y actualiza su estado (ok/roto y por qué).
func (e *Engine) SourceCall(ctx context.Context, sourceID, op string, args map[string]any) (json.RawMessage, error) {
	jar, err := e.Repo.JarFor(sourceID)
	if err != nil {
		return nil, err
	}
	if err := e.wait(ctx, sourceID); err != nil {
		return nil, err
	}
	if args == nil {
		args = map[string]any{}
	}
	args["source"] = sourceID
	res, err := e.Host.CallJar(ctx, jar, op, args)
	now := time.Now().Unix()
	if err == nil {
		e.St.DB.Exec(`UPDATE source SET status='ok', status_msg='', status_at=? WHERE id=?`, now, sourceID)
		return res, nil
	}
	switch k := hostproc.Kind(err); k {
	case "cloudflare", "webview", "javascript", "android", "http", "extension":
		e.St.DB.Exec(`UPDATE source SET status=?, status_msg=?, status_at=? WHERE id=?`, k, trunc(err.Error(), 300), now, sourceID)
	case "network":
		// fallo de red puntual: no marca el source como roto, pero deja constancia
		e.St.DB.Exec(`UPDATE source SET status_msg=?, status_at=? WHERE id=?`, trunc(err.Error(), 300), now, sourceID)
	}
	return nil, err
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

type Manga struct {
	URL         string `json:"url"`
	Title       string `json:"title"`
	Thumbnail   string `json:"thumbnail"`
	Author      string `json:"author"`
	Artist      string `json:"artist"`
	Description string `json:"description"`
	Genre       string `json:"genre"`
	Status      int    `json:"status"`
	// Memo: estado opaco de la extensión (extension-lib 1.6). Se guarda y se devuelve tal cual.
	Memo json.RawMessage `json:"memo,omitempty"`
}

type Chapter struct {
	URL       string          `json:"url"`
	Name      string          `json:"name"`
	Number    float64         `json:"number"`
	Date      int64           `json:"date"`
	Scanlator string          `json:"scanlator"`
	WebURL    string          `json:"webUrl"`
	Memo      json.RawMessage `json:"memo,omitempty"`
}

func (e *Engine) Search(ctx context.Context, sourceID, query string) ([]Manga, error) {
	op, args := "search", map[string]any{"query": query}
	if strings.TrimSpace(query) == "" {
		op = "popular"
	}
	res, err := e.SourceCall(ctx, sourceID, op, args)
	if err != nil {
		return nil, err
	}
	var r struct {
		Mangas []Manga `json:"mangas"`
	}
	json.Unmarshal(res, &r)
	return r.Mangas, nil
}

func (e *Engine) details(ctx context.Context, sourceID, url, title string, memo string) (*Manga, []Chapter, error) {
	mg := map[string]any{"url": url, "title": title}
	if memo != "" {
		mg["memo"] = json.RawMessage(memo)
	}
	res, err := e.SourceCall(ctx, sourceID, "details", map[string]any{"manga": mg})
	if err != nil {
		return nil, nil, err
	}
	var r struct {
		Manga    Manga     `json:"manga"`
		Chapters []Chapter `json:"chapters"`
	}
	if err := json.Unmarshal(res, &r); err != nil {
		return nil, nil, err
	}
	return &r.Manga, r.Chapters, nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
