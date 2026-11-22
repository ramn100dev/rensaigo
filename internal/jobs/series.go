package jobs

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"rensaigo/internal/i18n"
	"rensaigo/internal/library"
)

// Series: seguirlas, sus sources (el primero manda, los demás rellenan huecos) y la comprobación de capítulos nuevos.

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
