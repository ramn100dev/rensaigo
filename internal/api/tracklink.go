package api

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"rensaigo/internal/i18n"
	"rensaigo/internal/safe"
	"rensaigo/internal/track"
)

// Vincular series con los servicios de seguimiento y sincronizar lo leído en los dos sentidos.

// best busca una serie en el servicio y devuelve el mejor resultado y su parecido (contra todos sus títulos).
func best(ctx context.Context, t track.Service, title string) ([]track.Entry, int, float64, error) {
	es, err := t.Search(ctx, title)
	if err != nil {
		return nil, -1, 0, err
	}
	b, score := -1, 0.0
	for i := range es {
		for _, x := range es[i].Titles() {
			if sc := titleScore(title, x); sc > score {
				b, score = i, sc
			}
		}
	}
	return es, b, score, nil
}

// link vincula la serie, aplica los metadatos, trae lo leído en el servicio y sube lo leído aquí si va por delante.
func (s *Server) link(ctx context.Context, seriesID int64, t track.Service, e *track.Entry) {
	if full, err := t.Get(ctx, e.ID); err == nil { // la búsqueda no siempre trae lo leído
		e = full
	}
	s.St.DB.Exec(`INSERT INTO tracking(series_id, svc, rid, total, rd, pic) VALUES(?,?,?,?,?,?)
		ON CONFLICT(series_id, svc) DO UPDATE SET rid=excluded.rid, total=excluded.total, rd=excluded.rd, pic=excluded.pic`,
		seriesID, t.Key(), e.ID, e.Total, e.MyRead, e.Pic)
	s.touch(seriesID)
	s.applyMeta(seriesID, t.Key(), e)
	s.pull(seriesID, t.Key(), e.MyRead)
	safe.Go("sincronización", func() { s.push(seriesID, t, false) })
}

// applyMeta: los metadatos del servicio mandan sobre los del source, salvo que haya vinculado otro con más prioridad.
func (s *Server) applyMeta(seriesID int64, key string, e *track.Entry) {
	s.St.DB.Exec(`UPDATE tracking SET total=?, pic=CASE WHEN ?<>'' THEN ? ELSE pic END WHERE series_id=? AND svc=?`,
		e.Total, e.Pic, e.Pic, seriesID, key)
	for _, t := range s.Trk {
		if t.Key() == key {
			break
		}
		var n int
		if s.St.DB.QueryRow(`SELECT count(*) FROM tracking WHERE series_id=? AND svc=?`, seriesID, t.Key()).Scan(&n); n > 0 {
			return
		}
	}
	s.St.DB.Exec(`UPDATE series SET
		author=CASE WHEN ?<>'' THEN ? ELSE author END, description=CASE WHEN ?<>'' THEN ? ELSE description END,
		genre=CASE WHEN ?<>'' THEN ? ELSE genre END, status=CASE WHEN ?>0 THEN ? ELSE status END WHERE id=?`,
		e.Authors, e.Authors, e.Synopsis, e.Synopsis, e.Genres, e.Genres, e.Pub, e.Pub, seriesID)
}

// pull marca como leídos aquí los capítulos hasta el read del servicio. Solo suma: nunca desmarca nada.
func (s *Server) pull(seriesID int64, key string, read int) {
	if read <= 0 {
		return
	}
	s.touch(seriesID)
	res, err := s.St.DB.Exec(`INSERT INTO progress(chapter_id,page,done,updated)
		SELECT id, 0, 1, ? FROM chapter WHERE series_id=? AND number>0 AND number<=?
		ON CONFLICT(chapter_id) DO UPDATE SET done=1, updated=excluded.updated WHERE done=0`,
		time.Now().Unix(), seriesID, read)
	if err != nil {
		log.Printf("%s: marcar leídos de la serie %d: %v", key, seriesID, err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("%s: serie %d → %d capítulos marcados como leídos (hasta el %d)", key, seriesID, n, read)
	}
}

// push sube lo leído (el número de capítulo más alto terminado). Solo baja el contador si down es true, es decir,
// cuando lo has desmarcado tú a mano; los procesos automáticos nunca lo bajan.
func (s *Server) push(seriesID int64, t track.Service, down bool) {
	if !t.Connected() {
		return
	}
	s.trkMu.Lock() // de uno en uno: terminar dos capítulos seguidos no debe lanzar dos subidas que se crucen
	defer s.trkMu.Unlock()
	var rid, title string
	var total, synced int
	if s.St.DB.QueryRow(`SELECT k.rid, k.total, k.rd, s.title FROM tracking k JOIN series s ON s.id=k.series_id
		WHERE k.series_id=? AND k.svc=?`, seriesID, t.Key()).Scan(&rid, &total, &synced, &title) != nil {
		return
	}
	var maxNum float64
	s.St.DB.QueryRow(`SELECT coalesce(max(c.number),0) FROM chapter c JOIN progress p ON p.chapter_id=c.id
		WHERE c.series_id=? AND p.done=1`, seriesID).Scan(&maxNum)
	read := int(maxNum)
	if total > 0 && read > total {
		read = total
	}
	if read == synced || (read < synced && !down) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if !down {
		// antes de subir, mirar el servicio: si allí ya vas igual o más adelante (leído en el móvil, en otra app…),
		// se trae eso en vez de pisarlo con un número menor
		if e, err := t.Get(ctx, rid); err == nil && e.MyRead >= read {
			s.St.DB.Exec(`UPDATE tracking SET rd=? WHERE series_id=? AND svc=?`, e.MyRead, seriesID, t.Key())
			s.pull(seriesID, t.Key(), e.MyRead)
			return
		}
	}
	if err := t.Push(ctx, rid, read, total); err != nil {
		s.St.Event(t.Key(), "%s: no se pudo actualizar «%s»: %v", t.Name(), title, err)
		return
	}
	s.St.DB.Exec(`UPDATE tracking SET rd=? WHERE series_id=? AND svc=?`, read, seriesID, t.Key())
	log.Printf("%s: «%s» → %d capítulos leídos", t.Key(), title, read)
}

// syncSeries sube lo leído de una serie a todos los servicios con los que está vinculada.
func (s *Server) syncSeries(seriesID int64, down bool) {
	for _, t := range s.Trk {
		s.push(seriesID, t, down)
	}
}

func (s *Server) trkBoxes(seriesID int64, extra map[string]M) []M {
	var out []M
	for _, t := range s.Trk {
		if b := s.trkBox(seriesID, t, extra[t.Key()]); b != nil {
			out = append(out, b)
		}
	}
	return out
}

// trkBox: la fila de un servicio en la ficha de la serie (nil si ni está conectado ni vinculado).
func (s *Server) trkBox(seriesID int64, t track.Service, extra M) M {
	l := s.row(`SELECT rid, total, rd FROM tracking WHERE series_id=? AND svc=?`, seriesID, t.Key())
	if l == nil && !t.Connected() {
		return nil
	}
	d := M{"Key": t.Key(), "Name": t.Name(), "Look": trkLook[t.Key()], "On": t.Connected(), "SID": seriesID, "L": l}
	if l != nil {
		d["URL"] = webURL(t.Key(), fmt.Sprint(l["rid"]))
	}
	for k, v := range extra {
		d[k] = v
	}
	return d
}

func (s *Server) renderBox(w http.ResponseWriter, id int64, t track.Service, extra M) {
	if b := s.trkBox(id, t, extra); b != nil {
		s.render(w, "track_box", b)
	} // si no: ni conectado ni vinculado, la fila desaparece
}

// seriesTrkLink: con rid enlaza esa ficha; sin él busca el título y enlaza si hay una clara, o enseña candidatos.
func (s *Server) seriesTrkLink(w http.ResponseWriter, r *http.Request) {
	t := s.svcFor(w, r)
	if t == nil {
		return
	}
	id := pathInt(r, "id")
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	if rid := r.FormValue("rid"); rid != "" {
		e, err := t.Get(ctx, rid)
		if err != nil {
			s.renderBox(w, id, t, M{"Err": err.Error()})
			return
		}
		s.link(ctx, id, t, e)
		setToast(w, i18n.Tf("Vinculada con «%s» de %s", e.Title, t.Name()))
		s.renderBox(w, id, t, nil)
		return
	}
	var title string
	s.St.DB.QueryRow(`SELECT title FROM series WHERE id=?`, id).Scan(&title)
	es, b, score, err := best(ctx, t, title)
	if err != nil {
		s.renderBox(w, id, t, M{"Err": err.Error()})
		return
	}
	if b >= 0 && score >= 0.9 {
		s.link(ctx, id, t, &es[b])
		setToast(w, i18n.Tf("Vinculada con «%s» de %s", es[b].Title, t.Name()))
		s.renderBox(w, id, t, nil)
		return
	}
	if len(es) == 0 {
		s.renderBox(w, id, t, M{"Err": i18n.Tf("No aparece en %s", t.Name())})
		return
	}
	s.renderBox(w, id, t, M{"Cands": es})
}

func (s *Server) seriesTrkUnlink(w http.ResponseWriter, r *http.Request) {
	t := s.svcFor(w, r)
	if t == nil {
		return
	}
	id := pathInt(r, "id")
	s.St.DB.Exec(`DELETE FROM tracking WHERE series_id=? AND svc=?`, id, t.Key())
	s.touch(id)
	s.renderBox(w, id, t, nil)
}

// trkLinkAll vincula de una vez las series sin vincular que tengan una coincidencia clara.
func (s *Server) trkLinkAll(w http.ResponseWriter, r *http.Request) {
	t := s.svcFor(w, r)
	if t == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
	defer cancel()
	var done, miss []string
	for _, se := range s.rows(`SELECT id, title FROM series WHERE id NOT IN (SELECT series_id FROM tracking WHERE svc=?) ORDER BY title`, t.Key()) {
		title := se["title"].(string)
		es, b, score, err := best(ctx, t, title)
		if err != nil {
			s.renderPanel(w, r, t, M{"Err": err.Error()})
			return
		}
		if b >= 0 && score >= 0.9 {
			s.link(ctx, se["id"].(int64), t, &es[b])
			done = append(done, title)
		} else {
			miss = append(miss, title)
		}
	}
	msg := i18n.Tf("%d series vinculadas", len(done))
	if len(miss) > 0 {
		msg += " · " + i18n.Tf("%d sin coincidencia clara (vincúlalas desde su ficha): %s", len(miss), strings.Join(miss, ", "))
	}
	s.renderPanel(w, r, t, M{"Msg": msg})
}

// TrackLoop, al arrancar y luego una vez al día, refresca los metadatos de las series vinculadas, trae lo que haya
// cambiado en cada servicio (incluidos capítulos que hayan aparecido desde la última vez) y sube lo que vaya por
// delante aquí.
func (s *Server) TrackLoop(ctx context.Context) {
	for {
		// una vez al día aunque se reinicie el contenedor (la hora de la última pasada queda guardada)
		last, _ := strconv.ParseInt(s.St.Get("track.last_sync", "0"), 10, 64)
		wait := max(time.Until(time.Unix(last, 0).Add(24*time.Hour)), time.Minute)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		s.St.Set("track.last_sync", strconv.FormatInt(time.Now().Unix(), 10))
		s.trackSyncAll(ctx)
	}
}

// trackSyncAll: la pasada diaria. Trae de cada servicio lo que haya cambiado allí y sube lo que vaya por delante aquí.
func (s *Server) trackSyncAll(ctx context.Context) {
	for _, t := range s.Trk {
		if !t.Connected() {
			continue
		}
		for _, k := range s.rows(`SELECT series_id, rid, rd FROM tracking WHERE svc=?`, t.Key()) {
			id, last := k["series_id"].(int64), int(k["rd"].(int64))
			c, cancel := context.WithTimeout(ctx, time.Minute)
			e, err := t.Get(c, k["rid"].(string))
			cancel()
			if err != nil {
				continue
			}
			s.applyMeta(id, t.Key(), e)
			// solo se trae lo que haya cambiado en el servicio desde la última vez: así un capítulo desmarcado aquí
			// no vuelve a marcarse por el valor viejo del servicio
			if e.MyRead != last {
				s.St.DB.Exec(`UPDATE tracking SET rd=? WHERE series_id=? AND svc=?`, e.MyRead, id, t.Key())
				if e.MyRead > last {
					s.pull(id, t.Key(), e.MyRead)
				}
			}
			s.push(id, t, false) // por si aquí vas más adelantado
		}
	}
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// trackCover sirve la portada del servicio vinculado con más prioridad (cacheada en disco la primera vez).
// Devuelve false si no hay o falla.
func (s *Server) trackCover(w http.ResponseWriter, r *http.Request, id int64) bool {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	p := s.trackCoverFile(ctx, id)
	if p == "" {
		return false
	}
	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.ServeFile(w, r, p)
	return true
}

// trackCoverFile: la portada oficial de la serie (MAL/AniList/MangaDex) en la caché local, bajándola si hace falta.
// "" si no está vinculada o no se pudo bajar.
func (s *Server) trackCoverFile(ctx context.Context, id int64) string {
	for _, t := range s.Trk {
		var pic, rid string
		if s.St.DB.QueryRow(`SELECT pic, rid FROM tracking WHERE series_id=? AND svc=?`, id, t.Key()).Scan(&pic, &rid) != nil ||
			pic == "" || !strings.HasPrefix(pic, "https://") {
			continue
		}
		p := s.Cfg.Path("cache", "covers", t.Key()+"-"+unsafeName.ReplaceAllString(rid, "_")+".jpg")
		if _, err := os.Stat(p); err != nil {
			b, err := fetchImage(ctx, pic, "")
			if err != nil {
				continue
			}
			os.MkdirAll(filepath.Dir(p), 0o755)
			if os.WriteFile(p+".tmp", b, 0o644) != nil || os.Rename(p+".tmp", p) != nil {
				continue
			}
		}
		return p
	}
	return ""
}

func fetchImage(ctx context.Context, url, referer string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "rensaigo")
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 || len(b) == 0 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return b, nil
}
