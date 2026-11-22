package api

// Servicios de seguimiento (MyAnimeList, AniList, MangaDex), todos con el mismo funcionamiento:
//   - se conectan desde Ajustes → Conexiones;
//   - cada serie se vincula a su ficha (a mano desde la ficha de la serie, en bloque desde Conexiones o sola al
//     añadirla desde una lista); los vínculos van en la tabla tracking;
//   - lo leído va en los dos sentidos: al terminar un capítulo se sube; lo que cambie en el servicio se trae aquí
//     (al vincular y una vez al día). rd guarda el último valor acordado, para saber de qué lado ha cambiado;
//   - los metadatos (portada, sinopsis, géneros, autores, estado) pasan a ser los del servicio con más prioridad
//     de los vinculados (MAL, luego AniList, luego MangaDex);
//   - desde «Añadir» se ve la lista del usuario y las elegidas se buscan en todos los sources.

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"rensaigo/internal/jobs"
	"rensaigo/internal/track"
)

// aspecto de cada servicio en la UI
var trkLook = map[string]M{
	"mal":      {"s": "MAL", "c": "#2e51a2"},
	"anilist":  {"s": "AL", "c": "#02a9ff"},
	"mangadex": {"s": "MD", "c": "#ff6740"},
}

func (s *Server) trackRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /connections", s.connections)
	mux.HandleFunc("POST /track/{svc}/credentials", s.trkCredentials)
	mux.HandleFunc("GET /track/{svc}/connect", s.trkConnect)
	mux.HandleFunc("GET /mal/callback", s.trkCallback("mal")) // la dirección registrada en la app de MAL
	mux.HandleFunc("GET /anilist/callback", s.trkCallback("anilist"))
	mux.HandleFunc("POST /track/mangadex/login", s.mdLogin)
	mux.HandleFunc("POST /track/{svc}/disconnect", s.trkDisconnect)
	mux.HandleFunc("POST /track/{svc}/link-all", s.trkLinkAll)
	mux.HandleFunc("GET /track/list", s.trkList)
	mux.HandleFunc("POST /track/{svc}/search/{rid}", s.trkSearch)
	mux.HandleFunc("POST /track/{svc}/import/{rid}", s.trkImport)
	mux.HandleFunc("POST /series/{id}/track/{svc}", s.seriesTrkLink)
	mux.HandleFunc("POST /series/{id}/track/{svc}/unlink", s.seriesTrkUnlink)
}

func (s *Server) svc(key string) track.Service {
	for _, t := range s.Trk {
		if t.Key() == key {
			return t
		}
	}
	return nil
}

// svcFor saca el servicio de la ruta; si no existe responde 404 y devuelve nil.
func (s *Server) svcFor(w http.ResponseWriter, r *http.Request) track.Service {
	t := s.svc(r.PathValue("svc"))
	if t == nil {
		http.NotFound(w, r)
	}
	return t
}

func (s *Server) anyTracker() bool {
	for _, t := range s.Trk {
		if t.Connected() {
			return true
		}
	}
	return false
}

func (s *Server) connectedCount() int {
	n := 0
	for _, t := range s.Trk {
		if t.Connected() {
			n++
		}
	}
	return n
}

// webURL: la ficha de un vínculo en la web del servicio.
func webURL(svc, rid string) string {
	switch svc {
	case "mal":
		return "https://myanimelist.net/manga/" + rid
	case "anilist":
		return "https://anilist.co/manga/" + rid
	case "mangadex":
		return "https://mangadex.org/title/" + rid
	}
	return ""
}

// callbackURI: la dirección a la que vuelve el servicio tras iniciar sesión. Tiene que coincidir con la registrada en
// la app del servicio, así que se enseña en Conexiones tal cual.
func callbackURI(r *http.Request, svc string) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	return scheme + "://" + host + "/" + svc + "/callback"
}

// ---------------------------------------------------------------- Conexiones

func (s *Server) connections(w http.ResponseWriter, r *http.Request) {
	var ps []M
	for _, t := range s.Trk {
		ps = append(ps, s.panelData(r, t))
	}
	s.render(w, "connections", M{"Panels": ps, "OPDS": s.opdsKey() != "", "Notify": len(s.channels())})
}

func (s *Server) panelData(r *http.Request, t track.Service) M {
	var linked, total int
	s.St.DB.QueryRow(`SELECT count(*) FROM tracking WHERE svc=?`, t.Key()).Scan(&linked)
	s.St.DB.QueryRow(`SELECT count(*) FROM series`).Scan(&total)
	d := M{"Key": t.Key(), "Name": t.Name(), "Look": trkLook[t.Key()], "Connected": t.Connected(), "User": t.User(),
		"Linked": linked, "Unlinked": total - linked, "Redirect": callbackURI(r, t.Key())}
	if o, ok := t.(track.OAuth); ok {
		d["ClientID"], d["HasSecret"] = o.ClientID(), o.HasSecret()
	}
	if md, ok := t.(*track.MangaDex); ok {
		d["ClientID"], d["HasSecret"] = md.ClientID(), md.HasSecret()
	}
	if al, ok := t.(*track.AniList); ok && al.Expired() {
		d["Err"] = "La sesión de AniList caducó (dura un año): vuelve a conectar."
	}
	return d
}

func (s *Server) renderPanel(w http.ResponseWriter, r *http.Request, t track.Service, extra M) {
	d := s.panelData(r, t)
	for k, v := range extra {
		d[k] = v
	}
	s.render(w, "track_panel", d)
}

func (s *Server) trkCredentials(w http.ResponseWriter, r *http.Request) {
	t := s.svcFor(w, r)
	if t == nil {
		return
	}
	o, ok := t.(track.OAuth)
	if !ok {
		http.NotFound(w, r)
		return
	}
	id := strings.TrimSpace(r.FormValue("client_id"))
	if id == "" {
		s.renderPanel(w, r, t, M{"Err": "Pega el Client ID de tu app de " + t.Name()})
		return
	}
	o.SetCredentials(id, r.FormValue("client_secret"))
	if _, err := o.AuthURL(callbackURI(r, t.Key())); err != nil {
		s.renderPanel(w, r, t, M{"Err": err.Error()})
		return
	}
	w.Header().Set("HX-Redirect", "/track/"+t.Key()+"/connect") // salta al servicio para iniciar sesión
}

func (s *Server) trkConnect(w http.ResponseWriter, r *http.Request) {
	t := s.svcFor(w, r)
	if t == nil {
		return
	}
	o, ok := t.(track.OAuth)
	if !ok {
		http.NotFound(w, r)
		return
	}
	redir := callbackURI(r, t.Key())
	s.St.Set(t.Key()+".redirect", redir)
	u, err := o.AuthURL(redir)
	if err != nil {
		http.Redirect(w, r, "/system#conexiones", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, u, http.StatusSeeOther)
}

func (s *Server) trkCallback(key string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t := s.svc(key)
		o, _ := t.(track.OAuth)
		if o == nil {
			http.NotFound(w, r)
			return
		}
		if e := r.FormValue("error"); e != "" {
			s.St.Event(key, "inicio de sesión cancelado: %s", e)
			http.Redirect(w, r, "/system#conexiones", http.StatusSeeOther)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
		defer cancel()
		if err := o.Exchange(ctx, r.FormValue("code"), r.FormValue("state"), s.St.Get(key+".redirect", callbackURI(r, key))); err != nil {
			s.St.Event(key, "no se pudo conectar %s: %v", t.Name(), err)
		} else {
			s.St.Event(key, "%s conectado como %s", t.Name(), t.User())
		}
		http.Redirect(w, r, "/system#conexiones", http.StatusSeeOther)
	}
}

func (s *Server) mdLogin(w http.ResponseWriter, r *http.Request) {
	t := s.svc("mangadex")
	md := t.(*track.MangaDex)
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	if err := md.Login(ctx, r.FormValue("username"), r.FormValue("password"), r.FormValue("client_id"), r.FormValue("client_secret")); err != nil {
		s.renderPanel(w, r, t, M{"Err": err.Error()})
		return
	}
	s.St.Event("mangadex", "MangaDex conectado como %s", md.User())
	setToast(w, "MangaDex conectado")
	s.renderPanel(w, r, t, nil)
}

func (s *Server) trkDisconnect(w http.ResponseWriter, r *http.Request) {
	t := s.svcFor(w, r)
	if t == nil {
		return
	}
	t.Disconnect()
	s.St.Event(t.Key(), "%s desconectado", t.Name())
	s.renderPanel(w, r, t, nil)
}

// ---------------------------------------------------------------- vincular series

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
	go s.push(seriesID, t, false)
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
		setToast(w, "Vinculada con «"+e.Title+"» de "+t.Name())
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
		setToast(w, "Vinculada con «"+es[b].Title+"» de "+t.Name())
		s.renderBox(w, id, t, nil)
		return
	}
	if len(es) == 0 {
		s.renderBox(w, id, t, M{"Err": "No aparece en " + t.Name()})
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
	msg := fmt.Sprintf("%d series vinculadas", len(done))
	if len(miss) > 0 {
		msg += fmt.Sprintf(" · %d sin coincidencia clara (vincúlalas desde su ficha): %s", len(miss), strings.Join(miss, ", "))
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
		for _, t := range s.Trk {
			if !t.Connected() {
				continue
			}
			for _, k := range s.rows(`SELECT series_id, rid, rd FROM tracking WHERE svc=?`, t.Key()) {
				id, last := k["series_id"].(int64), int(k["rd"].(int64))
				c, cancel := context.WithTimeout(ctx, time.Minute)
				e, err := t.Get(c, k["rid"].(string))
				cancel()
				if err == nil {
					s.applyMeta(id, t.Key(), e)
					// solo se trae lo que haya cambiado en el servicio desde la última vez: así un capítulo
					// desmarcado aquí no vuelve a marcarse por el valor viejo del servicio
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

// ---------------------------------------------------------------- añadir desde la lista del usuario

func (s *Server) trkList(w http.ResponseWriter, r *http.Request) {
	var on []track.Service
	for _, t := range s.Trk {
		if t.Connected() {
			on = append(on, t)
		}
	}
	d := M{}
	if len(on) == 0 {
		d["Err"] = "Conecta una cuenta en Ajustes → Conexiones."
		s.render(w, "track_list", d)
		return
	}
	t := on[0]
	for _, x := range on {
		if x.Key() == r.FormValue("svc") {
			t = x
		}
	}
	sts := t.Statuses()
	st := sts[0].V
	for _, x := range sts {
		if x.V == r.FormValue("status") {
			st = x.V
		}
	}
	var svcs []M
	for _, x := range on {
		svcs = append(svcs, M{"v": x.Key(), "l": x.Name()})
	}
	d = M{"Svcs": svcs, "Svc": t.Key(), "Name": t.Name(), "Statuses": sts, "Status": st}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	list, err := t.List(ctx, st)
	if err != nil {
		d["Err"] = err.Error()
		s.render(w, "track_list", d)
		return
	}
	have := map[string]bool{}
	for _, x := range s.rows(`SELECT rid FROM tracking WHERE svc=?`, t.Key()) {
		have[x["rid"].(string)] = true
	}
	var rows []M
	inLib := 0
	for i := range list {
		if have[list[i].ID] {
			inLib++
			continue
		}
		rows = append(rows, M{"M": &list[i], "Svc": t.Key()})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return strings.ToLower(rows[i]["M"].(*track.Entry).Title) < strings.ToLower(rows[j]["M"].(*track.Entry).Title)
	})
	d["Rows"], d["InLib"] = rows, inLib
	s.render(w, "track_list", d)
}

// trkSearch busca una entrada de la lista en todos los sources y devuelve los candidatos para elegir; la coincidencia
// clara viene ya marcada. No añade nada: eso lo hace trkImport con el candidato elegido.
func (s *Server) trkSearch(w http.ResponseWriter, r *http.Request) {
	t := s.svcFor(w, r)
	if t == nil {
		return
	}
	rid := r.PathValue("rid")
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	s.importSlot <- struct{}{} // como mucho dos a la vez, aunque se elijan cincuenta
	defer func() { <-s.importSlot }()
	e, err := t.Get(ctx, rid)
	if err != nil {
		s.render(w, "track_row", M{"Row": M{"M": &track.Entry{ID: rid, Title: r.FormValue("t")}, "Svc": t.Key(), "Err": err.Error(), "Sel": true}})
		return
	}
	titles := e.Titles()
	if len(titles) > 2 {
		titles = titles[:2] // título principal e inglés: los demás rara vez ayudan y multiplican las búsquedas
	}
	cands, fails := s.searchAll(ctx, titles)
	if len(cands) > 10 {
		cands = cands[:10]
	}
	row := M{"M": e, "Svc": t.Key(), "Sel": true, "Searched": true, "Cands": cands, "Fails": strings.Join(fails, ", "), "Picked": -1}
	if len(cands) > 0 && cands[0].Score >= 0.85 {
		row["Picked"] = 0
	}
	if len(cands) == 0 {
		row["Err"] = "No aparece en tus sources"
	}
	s.render(w, "track_row", M{"Row": row})
}

// trkImport añade a la biblioteca el candidato elegido (source+url) y lo vincula con la entrada de la lista.
func (s *Server) trkImport(w http.ResponseWriter, r *http.Request) {
	t := s.svcFor(w, r)
	if t == nil {
		return
	}
	rid := r.PathValue("rid")
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	s.importSlot <- struct{}{}
	defer func() { <-s.importSlot }()
	e, err := t.Get(ctx, rid)
	if err != nil {
		s.render(w, "track_row", M{"Row": M{"M": &track.Entry{ID: rid, Title: r.FormValue("t")}, "Svc": t.Key(), "Err": err.Error()}})
		return
	}
	row := M{"M": e, "Svc": t.Key()}
	src := r.FormValue("source")
	jm := jobs.Manga{URL: r.FormValue("url"), Title: r.FormValue("title"), Thumbnail: r.FormValue("thumb"), Memo: []byte(r.FormValue("memo"))}
	if jm.URL == "" {
		http.Error(w, "falta el candidato", 400)
		return
	}
	id, err := s.Eng.Follow(ctx, src, jm, s.Cfg.DefaultCat, true)
	if err != nil && id == 0 {
		row["Err"] = friendlyErr(err)
		s.render(w, "track_row", M{"Row": row})
		return
	}
	s.link(ctx, id, t, e)
	var name string
	s.St.DB.QueryRow(`SELECT name FROM source WHERE id=?`, src).Scan(&name)
	row["Done"], row["SeriesID"] = name, id
	libChanged(w, "«"+e.Title+"» añadida a la biblioteca")
	s.render(w, "track_row", M{"Row": row})
}
