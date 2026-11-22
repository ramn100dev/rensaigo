// Package jobs: llamadas a sources (con rate-limit y estado), seguimiento de series, planificador y cola de descargas.
package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"rensaigo/internal/i18n"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "golang.org/x/image/webp"

	"rensaigo/internal/config"
	"rensaigo/internal/extrepo"
	"rensaigo/internal/hostproc"
	"rensaigo/internal/library"
	"rensaigo/internal/store"
)

type Engine struct {
	Cfg  *config.Config
	St   *store.Store
	Host *hostproc.Supervisor
	Repo *extrepo.Repo

	wake chan struct{}
	mu   sync.Mutex
	// descargas activas: capítulo -> progreso
	active  map[int64]*Active
	srcBusy map[string]int
	lim     map[string]*limiter
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
}

type limiter struct {
	mu   sync.Mutex
	next time.Time
}

func New(cfg *config.Config, st *store.Store, host *hostproc.Supervisor, repo *extrepo.Repo) *Engine {
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

// Follow crea (o reutiliza) una serie y le asocia un source. Devuelve el id de la serie.
func (e *Engine) Follow(ctx context.Context, sourceID string, m Manga, category string, auto bool) (int64, error) {
	if category == "" {
		category = e.Cfg.DefaultCat
	}
	title := strings.TrimSpace(m.Title)
	if title == "" {
		return 0, i18n.Errorf("título vacío")
	}
	dir := filepath.Join(e.Cfg.LibraryDir, library.SafeName(category), library.SafeName(title))
	var sid int64
	err := e.St.DB.QueryRow(`SELECT id FROM series WHERE path=?`, dir).Scan(&sid)
	if err == sql.ErrNoRows {
		res, err := e.St.DB.Exec(`INSERT INTO series(title,category,path,thumb_url,auto_download,created) VALUES(?,?,?,?,?,?)`,
			title, library.SafeName(category), dir, m.Thumbnail, b2i(auto), time.Now().Unix())
		if err != nil {
			return 0, err
		}
		sid, _ = res.LastInsertId()
	} else if err != nil {
		return 0, err
	}
	if err := e.AddSource(sid, sourceID, m.URL, title, string(m.Memo)); err != nil {
		return 0, err
	}
	e.St.Event("serie", "siguiendo «%s»", title)
	return sid, e.CheckSeries(ctx, sid)
}

// AddSource asocia otro source a la serie, con la prioridad más baja (número mayor = menos prioridad).
func (e *Engine) AddSource(seriesID int64, sourceID, url, title, memo string) error {
	var maxp sql.NullInt64
	e.St.DB.QueryRow(`SELECT max(priority) FROM series_source WHERE series_id=?`, seriesID).Scan(&maxp)
	p := int64(0)
	if maxp.Valid {
		p = maxp.Int64 + 1
	}
	_, err := e.St.DB.Exec(`INSERT OR IGNORE INTO series_source(series_id,source_id,url,title,priority,memo) VALUES(?,?,?,?,?,?)`,
		seriesID, sourceID, url, title, p, memo)
	e.NormSources(seriesID)
	return err
}

// NormSources deja los sources de una serie (0 = todas) en el modelo «el primero manda»: prioridades 0..n-1, el
// primero es el principal y los demás temporales (solo rellenan huecos; lo suyo se sustituye cuando el principal
// lo saca). También pone al día qué capítulos en disco cuentan como temporales y cancela las sustituciones
// pendientes que ya no van hacia el principal.
func (e *Engine) NormSources(seriesID int64) {
	q := `SELECT id, series_id FROM series_source ORDER BY series_id, priority, id`
	var args []any
	if seriesID > 0 {
		q = `SELECT id, series_id FROM series_source WHERE series_id=? ORDER BY priority, id`
		args = append(args, seriesID)
	}
	rows, err := e.St.DB.Query(q, args...)
	if err != nil {
		return
	}
	type row struct{ id, sid int64 }
	var list []row
	for rows.Next() {
		var r row
		rows.Scan(&r.id, &r.sid)
		list = append(list, r)
	}
	rows.Close()
	tx, err := e.St.DB.Begin()
	if err != nil {
		return
	}
	defer tx.Rollback()
	pos, last := 0, int64(-1)
	touched := map[int64]bool{}
	for _, r := range list {
		if r.sid != last {
			pos, last = 0, r.sid
		}
		tx.Exec(`UPDATE series_source SET priority=?, temporary=? WHERE id=?`, pos, b2i(pos > 0), r.id)
		touched[r.sid] = true
		pos++
	}
	if seriesID > 0 {
		touched[seriesID] = true // sin sources: lo que haya en disco deja de ser temporal
	}
	const principal = `(SELECT ss.id FROM series_source ss WHERE ss.series_id=chapter.series_id AND ss.temporary=0 LIMIT 1)`
	const fileFrom = `(source_name, lang) IN (SELECT src.name, src.lang FROM series_source ss JOIN source src ON src.id=ss.source_id
		WHERE ss.series_id=chapter.series_id AND ss.temporary=%d)`
	for sid := range touched {
		// lo bajado del principal no es temporal ni se sustituye. Lo de un source que se quitó de la serie
		// conserva lo que fuera: si era temporal, se sigue sustituyendo cuando lo saque el principal
		tx.Exec(`UPDATE chapter SET temp=0, upg=0, attempts=CASE WHEN upg=1 THEN 0 ELSE attempts END,
			error=CASE WHEN upg=1 THEN '' ELSE error END
			WHERE series_id=? AND state='downloaded' AND (temp=1 OR upg=1) AND `+fmt.Sprintf(fileFrom, 0), sid)
		// lo bajado de uno que ahora es temporal pasa a serlo
		tx.Exec(`UPDATE chapter SET temp=1 WHERE series_id=? AND state='downloaded' AND temp=0 AND upg=0
			AND `+fmt.Sprintf(fileFrom, 1)+` AND NOT `+fmt.Sprintf(fileFrom, 0), sid)
		// sustituciones pendientes hacia un source que ya no es el principal: se cancelan (el archivo se queda)
		tx.Exec(`UPDATE chapter SET upg=0, attempts=0, error='' WHERE series_id=? AND upg=1
			AND coalesce(series_source_id,0) <> coalesce(`+principal+`,0)`, sid)
	}
	tx.Commit()
}

// SetOrder ordena los sources de una serie (el primero pasa a ser el principal).
func (e *Engine) SetOrder(seriesID int64, ids []int64) error {
	tx, err := e.St.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// los que no vengan en la lista quedan detrás, en su orden
	tx.Exec(`UPDATE series_source SET priority=priority+1000 WHERE series_id=?`, seriesID)
	for i, id := range ids {
		tx.Exec(`UPDATE series_source SET priority=? WHERE id=? AND series_id=?`, i, id, seriesID)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	e.NormSources(seriesID)
	return nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

var (
	chapRe = regexp.MustCompile(`(?i)(?:cap[ií]tulo|cap\.?|chapter|ch\.?|episodio|ep\.?|#)\s*([0-9]+(?:[.,][0-9]+)?)`)
	numRe  = regexp.MustCompile(`([0-9]+(?:[.,][0-9]+)?)`)
)

// chapterNumber usa el número que da la extensión y, si no lo da, lo deduce del nombre.
func chapterNumber(c Chapter) float64 {
	if c.Number >= 0 {
		return c.Number
	}
	m := chapRe.FindStringSubmatch(c.Name)
	if m == nil {
		m = numRe.FindStringSubmatch(c.Name)
	}
	if m != nil {
		if f, err := strconv.ParseFloat(strings.Replace(m[1], ",", ".", 1), 64); err == nil {
			return f
		}
	}
	return -1
}

// CheckSeries consulta los sources de la serie por orden de prioridad y actualiza sus capítulos.
// Los capítulos se identifican por (serie, número): si ya existe uno (descargado o importado), no se duplica.
func (e *Engine) CheckSeries(ctx context.Context, seriesID int64) error {
	mu, _ := e.checkMu.LoadOrStore(seriesID, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	var title string
	var auto int
	if err := e.St.DB.QueryRow(`SELECT title, auto_download FROM series WHERE id=?`, seriesID).Scan(&title, &auto); err != nil {
		return err
	}
	type ss struct {
		id       int64
		sourceID string
		url      string
		title    string
		prio     int
		memo     string
		temp     int     // source temporal
		from     float64 // solo capítulos desde este número (0 = todos)
	}
	rows, err := e.St.DB.Query(`SELECT ss.id, ss.source_id, ss.url, coalesce(ss.title,''), ss.priority, ss.memo, ss.temporary, ss.from_ch FROM series_source ss
		JOIN source s ON s.id=ss.source_id WHERE ss.series_id=? AND s.enabled=1 ORDER BY ss.priority`, seriesID)
	if err != nil {
		return err
	}
	var list []ss
	for rows.Next() {
		var x ss
		rows.Scan(&x.id, &x.sourceID, &x.url, &x.title, &x.prio, &x.memo, &x.temp, &x.from)
		list = append(list, x)
	}
	rows.Close()
	now := time.Now().Unix()
	if len(list) == 0 {
		e.St.DB.Exec(`UPDATE series SET last_check=?, last_error='sin sources activos' WHERE id=?`, now, seriesID)
		return i18n.Errorf("la serie no tiene sources activos")
	}
	var firstErr error
	added, upgrades, okSources := 0, 0, 0
	for _, x := range list {
		m, chs, err := e.details(ctx, x.sourceID, x.url, x.title, x.memo)
		if err != nil {
			e.St.DB.Exec(`UPDATE series_source SET last_check=?, last_error=?, err_since=CASE WHEN err_since=0 THEN ? ELSE err_since END WHERE id=?`,
				now, trunc(err.Error(), 300), now, x.id)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		okSources++
		e.St.DB.Exec(`UPDATE series_source SET last_check=?, last_error='', err_since=0 WHERE id=?`, now, x.id)
		// completa metadatos que falten (el primero que los tenga)
		e.St.DB.Exec(`UPDATE series SET author=CASE WHEN author='' THEN ? ELSE author END, artist=CASE WHEN artist='' THEN ? ELSE artist END,
			description=CASE WHEN description='' THEN ? ELSE description END, genre=CASE WHEN genre='' THEN ? ELSE genre END,
			status=CASE WHEN status=0 THEN ? ELSE status END, thumb_url=CASE WHEN thumb_url='' THEN ? ELSE thumb_url END WHERE id=?`,
			m.Author, m.Artist, m.Description, m.Genre, m.Status, m.Thumbnail, seriesID)
		tx, err := e.St.DB.Begin()
		if err != nil {
			return err
		}
		// orden para quedarse con un capítulo: primero los sources no temporales, y entre ellos el de más prioridad
		rank := int64(x.prio) + 1_000_000*int64(x.temp)
		for _, c := range chs {
			n := chapterNumber(c)
			if x.from > 0 && n >= 0 && n < x.from {
				continue // por debajo del «desde el capítulo» de este source
			}
			var id int64
			var state string
			var curSS sql.NullInt64
			var curRank sql.NullInt64
			var ctemp, cupg int
			q := `SELECT c.id, c.state, c.series_source_id,
				(SELECT priority + 1000000*temporary FROM series_source WHERE id=c.series_source_id), c.temp, c.upg
				FROM chapter c WHERE c.series_id=? AND c.number=?`
			args := []any{seriesID, n}
			if n < 0 { // sin número fiable: se distingue por nombre
				q += ` AND c.name=?`
				args = append(args, c.Name)
			}
			err := tx.QueryRow(q+` ORDER BY (c.state='downloaded') DESC LIMIT 1`, args...).Scan(&id, &state, &curSS, &curRank, &ctemp, &cupg)
			switch {
			case err == sql.ErrNoRows:
				st := "available"
				if auto == 1 {
					st = "queued"
				}
				if _, err := tx.Exec(`INSERT OR IGNORE INTO chapter(series_id,number,name,state,series_source_id,url,web_url,scanlator,date_upload,memo,created)
					VALUES(?,?,?,?,?,?,?,?,?,?,?)`, seriesID, n, c.Name, st, x.id, c.URL, c.WebURL, c.Scanlator, c.Date/1000, string(c.Memo), now); err == nil {
					added++
				}
			case err != nil:
				tx.Rollback()
				return err
			case state == "downloaded":
				// ya está en disco: solo se enlaza con el source si aún no lo estaba (capítulos importados)
				if !curSS.Valid {
					tx.Exec(`UPDATE chapter SET series_source_id=?, url=?, memo=?, web_url=CASE WHEN web_url='' THEN ? ELSE web_url END,
						date_upload=CASE WHEN date_upload=0 THEN ? ELSE date_upload END WHERE id=?`, x.id, c.URL, string(c.Memo), c.WebURL, c.Date/1000, id)
				} else if ctemp == 1 && x.temp == 0 && auto == 1 &&
					((cupg == 0 && curSS.Int64 != x.id) || (cupg == 1 && (!curRank.Valid || curRank.Int64 > rank))) {
					// (nunca hacia el source del que ya vino: pasa si se marca temporal mientras se comprueba la serie)
					// el archivo vino de un source temporal y ya está en uno que no lo es: se baja de nuevo para
					// sustituirlo. Mientras tanto sigue en disco y se puede leer
					tx.Exec(`UPDATE chapter SET upg=1, series_source_id=?, url=?, web_url=?, scanlator=?, date_upload=?, memo=?,
						attempts=0, next_try=0, error='' WHERE id=?`, x.id, c.URL, c.WebURL, c.Scanlator, c.Date/1000, string(c.Memo), id)
					upgrades++
				}
			case state == "downloading":
			default:
				// pendiente: se queda con el source de mayor prioridad que lo tenga
				if !curSS.Valid || !curRank.Valid || curRank.Int64 > rank || curSS.Int64 == x.id {
					tx.Exec(`UPDATE chapter SET series_source_id=?, url=?, web_url=?, scanlator=?, date_upload=?, memo=? WHERE id=?`,
						x.id, c.URL, c.WebURL, c.Scanlator, c.Date/1000, string(c.Memo), id)
				}
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	msg := ""
	if okSources == 0 && firstErr != nil {
		msg = trunc(firstErr.Error(), 300)
	}
	e.St.DB.Exec(`UPDATE series SET last_check=?, last_error=? WHERE id=?`, now, msg, seriesID)
	if added > 0 {
		e.St.Event("comprobación", "«%s»: %d capítulos nuevos", title, added)
	}
	if upgrades > 0 {
		e.St.Event("comprobación", "«%s»: %d capítulos de un source temporal ya salen en el preferido: se sustituyen", title, upgrades)
	}
	if added > 0 || upgrades > 0 {
		e.Wake()
	}
	e.changed(seriesID)
	if okSources == 0 {
		return firstErr
	}
	return nil
}

// Queue marca capítulos para descargar. ids vacío = todos los pendientes de la serie.
func (e *Engine) Queue(seriesID int64, ids []int64) (int64, error) {
	var res sql.Result
	var err error
	if len(ids) == 0 {
		res, err = e.St.DB.Exec(`UPDATE chapter SET state='queued', attempts=0, next_try=0, error='' WHERE series_id=? AND state IN ('available','failed') AND series_source_id IS NOT NULL`, seriesID)
	} else {
		ph := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
		args := []any{seriesID}
		for _, id := range ids {
			args = append(args, id)
		}
		res, err = e.St.DB.Exec(`UPDATE chapter SET state='queued', attempts=0, next_try=0, error='' WHERE series_id=? AND state IN ('available','failed','queued') AND series_source_id IS NOT NULL AND id IN (`+ph+`)`, args...)
	}
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	e.Wake()
	return n, nil
}

// Recover deja el estado coherente tras un apagado brusco: lo que estaba "descargando" vuelve a la cola
// y se borran los temporales. Un CBZ solo existe con su nombre final si se escribió entero.
func (e *Engine) Recover() {
	e.NormSources(0) // series de antes del modelo «el primero manda»
	res, _ := e.St.DB.Exec(`UPDATE chapter SET state='queued', next_try=0 WHERE state='downloading'`)
	n, _ := res.RowsAffected()
	os.RemoveAll(e.Cfg.Path("tmp")) // páginas sueltas de capítulos a medias
	os.MkdirAll(e.Cfg.Path("tmp"), 0o755)
	parts := library.CleanParts(e.Cfg.LibraryDir)
	if n > 0 || parts > 0 {
		e.St.Event("recuperación", "%d capítulos a medias devueltos a la cola, %d ficheros .part eliminados", n, parts)
		log.Printf("recuperación: %d capítulos reencolados, %d .part eliminados", n, parts)
	}
}

// Run arranca planificador y cola. Termina cuando se cancela el contexto.
func (e *Engine) Run(ctx context.Context) {
	go e.scheduler(ctx)
	e.dispatcher(ctx)
}

func (e *Engine) scheduler(ctx context.Context) {
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		// Las series terminadas (el source dice «completado» o «publicación terminada») y con todo en disco ya no
		// esperan capítulos: se comprueban una vez por semana (por si hay extras o correcciones), no en cada ronda.
		// Así no se arranca el host ni se pregunta al sitio por ellas.
		now := time.Now()
		due, dueDone := now.Add(-e.Cfg.CheckInterval).Unix(), now.Add(-7*24*time.Hour).Unix()
		rows, err := e.St.DB.Query(`SELECT id FROM series WHERE paused=0 AND EXISTS(SELECT 1 FROM series_source WHERE series_id=series.id)
			AND last_check <= CASE WHEN coalesce(status,0) IN (2,4) AND EXISTS(SELECT 1 FROM chapter WHERE series_id=series.id)
				AND NOT EXISTS(SELECT 1 FROM chapter WHERE series_id=series.id AND state!='downloaded') THEN ? ELSE ? END
			ORDER BY last_check`, dueDone, due)
		if err != nil {
			continue
		}
		var ids []int64
		for rows.Next() {
			var id int64
			rows.Scan(&id)
			ids = append(ids, id)
		}
		rows.Close()
		if len(ids) == 0 {
			continue
		}
		for _, id := range ids {
			if ctx.Err() != nil {
				return
			}
			if err := e.CheckSeries(ctx, id); err != nil {
				log.Printf("planificador: serie %d: %v", id, err)
			}
		}
		e.mu.Lock()
		e.Checks++
		e.mu.Unlock()
		log.Printf("planificador: comprobadas %d series", len(ids))
	}
}

// CheckAll fuerza la comprobación de todas las series no pausadas.
func (e *Engine) CheckAll() {
	e.St.DB.Exec(`UPDATE series SET last_check=0 WHERE paused=0`)
}

func (e *Engine) dispatcher(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		e.dispatch(ctx)
		select {
		case <-ctx.Done():
			return
		case <-e.wake:
		case <-t.C:
		}
	}
}

// Paused: con las descargas en pausa no se empieza ningún capítulo (se guarda en ajustes: sigue tras reiniciar).
func (e *Engine) Paused() bool { return e.St.Get("dl.paused", "") == "1" }

func (e *Engine) SetPaused(p bool) {
	v := ""
	if p {
		v = "1"
	}
	e.St.Set("dl.paused", v)
	e.Wake()
}

// dispatch lanza descargas respetando el total (Cfg.DLConcurrency) y el máximo por source (Cfg.PerSource).
func (e *Engine) dispatch(ctx context.Context) {
	if e.Paused() {
		return
	}
	for ctx.Err() == nil {
		e.mu.Lock()
		free := e.Cfg.DLConcurrency - len(e.active)
		e.mu.Unlock()
		if free <= 0 {
			return
		}
		// Por turnos entre series: el 1.º pendiente de cada serie, luego el 2.º de cada una… Así una serie de 200
		// capítulos no deja a las demás esperando a que acabe (dentro de cada serie, por número; las sustituciones
		// de capítulos temporales, al final).
		rows, err := e.St.DB.Query(`SELECT c.id, ss.source_id, s.title, c.name, c.number, src.name, c.upg FROM
			(SELECT *, row_number() OVER (PARTITION BY series_id ORDER BY upg, number) AS rn FROM chapter
				WHERE (state='queued' OR (state='downloaded' AND upg=1)) AND next_try<=?) c
			JOIN series s ON s.id=c.series_id JOIN series_source ss ON ss.id=c.series_source_id JOIN source src ON src.id=ss.source_id
			WHERE s.paused=0 AND src.enabled=1
			ORDER BY c.rn, s.id LIMIT 60`, time.Now().Unix())
		if err != nil {
			return
		}
		type cand struct {
			id                int64
			source, title, ch string
			number            float64
			srcName           string
			upg               bool
		}
		var cs []cand
		for rows.Next() {
			var c cand
			rows.Scan(&c.id, &c.source, &c.title, &c.ch, &c.number, &c.srcName, &c.upg)
			cs = append(cs, c)
		}
		rows.Close()
		started := false
		e.mu.Lock()
		for _, c := range cs {
			if e.active[c.id] != nil || e.srcBusy[c.source] >= e.Cfg.PerSource {
				continue
			}
			label := c.ch
			if label == "" {
				label = "Cap. " + library.FormatNumber(c.number)
			}
			if c.upg {
				label += " (sustitución)"
			}
			a := &Active{ChapterID: c.id, Series: c.title, Chapter: label, Source: c.srcName}
			e.active[c.id] = a
			e.srcBusy[c.source]++
			if !c.upg { // una sustitución sigue «downloaded»: el capítulo viejo se puede leer mientras tanto
				e.St.DB.Exec(`UPDATE chapter SET state='downloading' WHERE id=?`, c.id)
			}
			go func(id int64, source string, a *Active) {
				err := e.download(ctx, id, a)
				e.mu.Lock()
				delete(e.active, id)
				e.srcBusy[source]--
				e.mu.Unlock()
				if err != nil && ctx.Err() == nil {
					e.fail(id, err)
				}
				e.Wake()
			}(c.id, c.source, a)
			started = true
			break
		}
		e.mu.Unlock()
		if !started {
			return
		}
	}
}

// fail aplica reintentos con espera creciente: 30 s, 2 min, 8 min, 32 min… hasta Cfg.MaxAttempts.
func (e *Engine) fail(id int64, err error) {
	var attempts, upg int
	e.St.DB.QueryRow(`SELECT attempts, upg FROM chapter WHERE id=?`, id).Scan(&attempts, &upg)
	attempts++
	msg := trunc(err.Error(), 300)
	if attempts >= e.Cfg.MaxAttempts && upg == 0 {
		e.St.DB.Exec(`UPDATE chapter SET state='failed', attempts=?, error=? WHERE id=?`, attempts, msg, id)
		e.St.Event("descarga", "capítulo %d fallido tras %d intentos: %s", id, attempts, msg)
		return
	}
	delay := 30 * time.Second
	for i := 1; i < attempts; i++ {
		delay *= 4
	}
	if delay > 6*time.Hour {
		delay = 6 * time.Hour
	}
	if hostproc.Kind(err) == "host" { // el host se cayó: no es culpa del source, se reintenta enseguida
		delay = 3 * time.Second
	}
	if upg == 1 {
		// una sustitución nunca pasa a «fallido»: el capítulo viejo sigue descargado y legible, y se reintenta
		// con espera creciente (hasta cada 6 h)
		if attempts == e.Cfg.MaxAttempts {
			e.St.Event("descarga", "la sustitución del capítulo %d sigue fallando (se reintentará cada pocas horas): %s", id, msg)
		}
		e.St.DB.Exec(`UPDATE chapter SET attempts=?, next_try=?, error=? WHERE id=?`, attempts, time.Now().Add(delay).Unix(), msg, id)
		return
	}
	e.St.DB.Exec(`UPDATE chapter SET state='queued', attempts=?, next_try=?, error=? WHERE id=?`,
		attempts, time.Now().Add(delay).Unix(), msg, id)
	log.Printf("descarga: capítulo %d falló (intento %d, reintento en %s): %s", id, attempts, delay, msg)
}

func (e *Engine) download(ctx context.Context, id int64, a *Active) error {
	var (
		seriesID                                   int64
		number                                     float64
		name, url, webURL, scanlator               string
		dateUpload                                 int64
		sTitle, sPath, author, artist, genre, desc string
		sourceID, srcName, lang                    string
		memo, oldCbz                               string
	)
	err := e.St.DB.QueryRow(`SELECT c.series_id, c.number, c.name, c.url, c.web_url, c.scanlator, c.date_upload,
		s.title, s.path, s.author, s.artist, s.genre, s.description, ss.source_id, src.name, src.lang, c.memo,
		CASE WHEN c.state='downloaded' AND c.upg=1 THEN c.cbz ELSE '' END
		FROM chapter c JOIN series s ON s.id=c.series_id JOIN series_source ss ON ss.id=c.series_source_id JOIN source src ON src.id=ss.source_id
		WHERE c.id=?`, id).Scan(&seriesID, &number, &name, &url, &webURL, &scanlator, &dateUpload,
		&sTitle, &sPath, &author, &artist, &genre, &desc, &sourceID, &srcName, &lang, &memo, &oldCbz)
	if err != nil {
		return err
	}
	tmp := e.Cfg.Path("tmp", fmt.Sprintf("ch-%d", id))
	os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	res, err := e.SourceCall(ctx, sourceID, "pages", map[string]any{"chapter": chapterArg(url, name, scanlator, number, dateUpload*1000, memo)})
	if err != nil {
		return err
	}
	var pr struct {
		Pages []map[string]any `json:"pages"`
	}
	json.Unmarshal(res, &pr)
	if len(pr.Pages) == 0 {
		return i18n.Errorf("el source no devolvió páginas")
	}
	e.mu.Lock()
	a.Total = len(pr.Pages)
	e.mu.Unlock()

	// Imágenes: SIEMPRE a través del host. Cfg.PageConcurrency a la vez dentro del capítulo.
	files := make([]string, len(pr.Pages))
	sem := make(chan struct{}, e.Cfg.PageConcurrency)
	var wg sync.WaitGroup
	var firstErr error
	var emu sync.Mutex
	dctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for i, pg := range pr.Pages {
		files[i] = filepath.Join(tmp, fmt.Sprintf("%04d.img", i))
		wg.Add(1)
		go func(i int, pg map[string]any) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-dctx.Done():
				return
			}
			var err error
			for try := 0; try < 3; try++ {
				if dctx.Err() != nil {
					return
				}
				_, err = e.SourceCall(dctx, sourceID, "image", map[string]any{"page": pg, "out": files[i]})
				if err == nil {
					break
				}
				select {
				case <-time.After(time.Duration(1+try*2) * time.Second):
				case <-dctx.Done():
					return
				}
			}
			if err != nil {
				emu.Lock()
				if firstErr == nil {
					firstErr = i18n.Errorf("página %d: %w", i+1, err)
				}
				emu.Unlock()
				cancel()
				return
			}
			e.mu.Lock()
			a.Done++
			e.mu.Unlock()
		}(i, pg)
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	info := &library.ComicInfo{
		Title: name, Series: sTitle, Number: strconv.FormatFloat(number, 'f', -1, 64), Summary: desc,
		Notes: "Created by rensaigo", Writer: author, Penciller: artist, Translator: firstNonEmpty(scanlator, srcName),
		Publisher: srcName, Tags: genre, Web: webURL, LanguageISO: lang, Format: "Web",
	}
	if number < 0 {
		info.Number = ""
	}
	if dateUpload > 0 {
		t := time.Unix(dateUpload, 0)
		info.Year, info.Month, info.Day = t.Year(), int(t.Month()), t.Day()
	}
	dest := filepath.Join(sPath, library.ChapterFile(srcName, lang, sTitle, name, number))
	size, err := library.WriteCBZ(dest, files, info)
	if err != nil {
		return fmt.Errorf("empaquetado: %w", err)
	}
	var mtime int64
	if fi, err := os.Stat(dest); err == nil {
		mtime = fi.ModTime().Unix()
	}
	_, err = e.St.DB.Exec(`UPDATE chapter SET state='downloaded', cbz=?, pages=?, bytes=?, cbz_mtime=?, source_name=?, lang=?, error='', attempts=0,
		upg=0, temp=coalesce((SELECT temporary FROM series_source WHERE id=chapter.series_source_id),0) WHERE id=?`,
		dest, len(files), size, mtime, srcName, lang, id)
	if err != nil {
		return err
	}
	if oldCbz != "" && oldCbz != dest { // sustitución: fuera el archivo del source temporal
		if err := os.Remove(oldCbz); err != nil && !os.IsNotExist(err) {
			log.Printf("sustitución: no se pudo borrar %s: %v", oldCbz, err)
		}
		e.St.Event("descarga", "«%s» capítulo %s sustituido por el de %s", sTitle, library.FormatNumber(number), srcName)
	}
	e.St.Event("descarga", "«%s» %s (%d páginas, %.1f MB)", sTitle, filepath.Base(dest), len(files), float64(size)/1e6)
	e.changed(seriesID)
	if oldCbz == "" && e.OnDownloaded != nil {
		e.OnDownloaded(seriesID, sTitle, number)
	}
	return nil
}

func firstNonEmpty(a ...string) string {
	for _, s := range a {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// Snapshot para la UI y las mediciones.
type Snapshot struct {
	Active []Active       `json:"active"`
	Counts map[string]int `json:"counts"`
	Checks int64          `json:"scheduler_runs"`
	// series con capítulos en cola
	QueuedSeries int `json:"queued_series"`
}

func (e *Engine) Snapshot() Snapshot {
	s := Snapshot{Counts: map[string]int{}}
	e.mu.Lock()
	for _, a := range e.active {
		s.Active = append(s.Active, *a)
	}
	s.Checks = e.Checks
	e.mu.Unlock()
	rows, err := e.St.DB.Query(`SELECT state, count(*) FROM chapter GROUP BY state`)
	if err == nil {
		for rows.Next() {
			var st string
			var n int
			rows.Scan(&st, &n)
			s.Counts[st] = n
		}
		rows.Close()
	}
	e.St.DB.QueryRow(`SELECT count(DISTINCT series_id) FROM chapter WHERE state IN ('queued','downloading')`).Scan(&s.QueuedSeries)
	return s
}

// chapterArg reconstruye el capítulo tal como lo dio la extensión (incluido su memo) para pedir sus páginas.
// Sample mide la calidad de un manga en un source sin descargarlo: cuántos capítulos tiene, el número del último y
// el ancho de una página de su último capítulo. Para comparar sources.
func (e *Engine) Sample(ctx context.Context, sourceID string, m Manga) (chapters int, last float64, width int, err error) {
	_, chs, err := e.details(ctx, sourceID, m.URL, m.Title, string(m.Memo))
	if err != nil {
		return 0, 0, 0, err
	}
	if len(chs) == 0 {
		return 0, 0, 0, nil
	}
	best := chs[0]
	for _, c := range chs {
		if n := chapterNumber(c); n > last {
			last, best = n, c
		}
	}
	res, err := e.SourceCall(ctx, sourceID, "pages", map[string]any{"chapter": chapterArg(best.URL, best.Name, best.Scanlator, chapterNumber(best), best.Date, string(best.Memo))})
	if err != nil {
		return len(chs), last, 0, nil // los capítulos sí se saben
	}
	var pr struct {
		Pages []map[string]any `json:"pages"`
	}
	json.Unmarshal(res, &pr)
	if len(pr.Pages) == 0 {
		return len(chs), last, 0, nil
	}
	pg := pr.Pages[len(pr.Pages)/2] // una del medio: la primera suele ser créditos o portada
	tmp := e.Cfg.Path("tmp", fmt.Sprintf("sample-%d.img", time.Now().UnixNano()))
	defer os.Remove(tmp)
	if _, err := e.SourceCall(ctx, sourceID, "image", map[string]any{"page": pg, "out": tmp}); err != nil {
		return len(chs), last, 0, nil
	}
	if f, err := os.Open(tmp); err == nil {
		if cfg, _, err := image.DecodeConfig(f); err == nil {
			width = cfg.Width
		}
		f.Close()
	}
	return len(chs), last, width, nil
}

func chapterArg(url, name, scanlator string, number float64, dateMs int64, memo string) map[string]any {
	c := map[string]any{"url": url, "name": name, "number": number, "date": dateMs}
	if scanlator != "" {
		c["scanlator"] = scanlator
	}
	if memo != "" {
		c["memo"] = json.RawMessage(memo)
	}
	return c
}
