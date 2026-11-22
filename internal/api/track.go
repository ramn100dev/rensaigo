package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"rensaigo/internal/i18n"
	"rensaigo/internal/track"
)

// Servicios de seguimiento (MyAnimeList, AniList, MangaDex), todos con el mismo funcionamiento:
//   - se conectan desde Ajustes → Conexiones;
//   - cada serie se vincula a su ficha (a mano desde la ficha de la serie, en bloque desde Conexiones o sola al
//     añadirla desde una lista); los vínculos van en la tabla tracking;
//   - lo leído va en los dos sentidos: al terminar un capítulo se sube; lo que cambie en el servicio se trae aquí
//     (al vincular y una vez al día). rd guarda el último valor acordado, para saber de qué lado ha cambiado;
//   - los metadatos (portada, sinopsis, géneros, autores, estado) pasan a ser los del servicio con más prioridad
//     de los vinculados (MAL, luego AniList, luego MangaDex);
//   - desde «Añadir» se ve la lista del usuario y las elegidas se buscan en todos los sources.

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
		d["Err"] = i18n.T("La sesión de AniList caducó (dura un año): vuelve a conectar.")
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
		s.renderPanel(w, r, t, M{"Err": i18n.Tf("Pega el Client ID de tu app de %s", t.Name())})
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
	setToast(w, i18n.T("MangaDex conectado"))
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
