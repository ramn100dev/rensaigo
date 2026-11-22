package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"rensaigo/internal/hostproc"
	"rensaigo/internal/i18n"
	"rensaigo/internal/library"
	"rensaigo/internal/safe"
)

// Cola de descargas: planificador de comprobaciones, reparto de descargas por turnos, reintentos y la descarga
// de cada capítulo a CBZ.

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
	go safe.Loop(ctx, "planificador", e.scheduler)
	safe.Loop(ctx, "cola de descargas", e.dispatcher)
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
		rows, err := e.St.DB.Query(`SELECT c.id, ss.source_id, s.title, c.name, c.number, src.name, c.upg, c.series_id, c.rn FROM
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
			series, rn        int64
		}
		var cs []cand
		for rows.Next() {
			var c cand
			rows.Scan(&c.id, &c.source, &c.title, &c.ch, &c.number, &c.srcName, &c.upg, &c.series, &c.rn)
			cs = append(cs, c)
		}
		rows.Close()
		started := false
		e.mu.Lock()
		// rn vuelve a 1 cuando una serie termina un capítulo, así que a igual rn va primero la serie que lleva más
		// tiempo sin empezar ninguno: si no, la de id más bajo se llevaría todos los turnos de su source
		if e.served == nil {
			e.served = map[int64]int64{}
		}
		sort.SliceStable(cs, func(i, j int) bool {
			if cs[i].rn != cs[j].rn {
				return cs[i].rn < cs[j].rn
			}
			return e.served[cs[i].series] < e.served[cs[j].series]
		})
		for _, c := range cs {
			if e.active[c.id] != nil || e.srcBusy[c.source] >= e.Cfg.PerSource {
				continue
			}
			label := c.ch
			if label == "" {
				label = i18n.Tf("Capítulo %s", library.FormatNumber(c.number))
			}
			if c.upg {
				label += " " + i18n.T("(sustitución)")
			}
			e.turn++
			e.served[c.series] = e.turn
			dctx, dcancel := context.WithCancel(ctx)
			a := &Active{ChapterID: c.id, Series: c.title, Chapter: label, Source: c.srcName, series: c.series, cancel: dcancel}
			e.active[c.id] = a
			e.srcBusy[c.source]++
			if !c.upg { // una sustitución sigue «downloaded»: el capítulo viejo se puede leer mientras tanto
				e.St.DB.Exec(`UPDATE chapter SET state='downloading' WHERE id=?`, c.id)
			}
			go func(id int64, source string, a *Active) {
				err := safe.Call("descarga", func() error { return e.download(dctx, id, a) })
				cancelled := dctx.Err() != nil
				dcancel()
				e.mu.Lock()
				delete(e.active, id)
				e.srcBusy[source]--
				e.mu.Unlock()
				if err != nil && !cancelled && !errors.Is(err, errGone) {
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
	// el host se cayó (o no arrancó): no es culpa del source, se reintenta enseguida. Ojo: hostproc.Kind da «host»
	// también para cualquier error que no venga del host (p. ej. «el source no devolvió páginas»), y esos sí
	// tienen que esperar cada vez más
	if he := (*hostproc.Error)(nil); errors.As(err, &he) && he.Kind == "host" {
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
	if err == sql.ErrNoRows {
		return errGone // se borró (o se quitó su source) justo antes de empezar
	} else if err != nil {
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
			defer safe.Recover("página")
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
	var other int64
	if e.St.DB.QueryRow(`SELECT id FROM chapter WHERE cbz=? AND id<>?`, dest, id).Scan(&other) == nil {
		// el nombre ya es de otro capítulo (títulos muy largos o repetidos): no se pisa su archivo
		dest = strings.TrimSuffix(dest, ".cbz") + fmt.Sprintf(" [%d].cbz", id)
	}
	if e.St.DB.QueryRow(`SELECT id FROM chapter WHERE id=?`, id).Scan(&other) != nil {
		return errGone
	}
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
