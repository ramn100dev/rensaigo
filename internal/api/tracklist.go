package api

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"rensaigo/internal/i18n"
	"rensaigo/internal/jobs"
	"rensaigo/internal/track"
)

// Añadir series desde la lista del usuario en un servicio de seguimiento.

func (s *Server) trkList(w http.ResponseWriter, r *http.Request) {
	var on []track.Service
	for _, t := range s.Trk {
		if t.Connected() {
			on = append(on, t)
		}
	}
	d := M{}
	if len(on) == 0 {
		d["Err"] = i18n.T("Conecta una cuenta en Ajustes → Conexiones.")
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
	for i := range sts {
		sts[i].L = i18n.T(sts[i].L)
	}
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
		row["Err"] = i18n.T("No aparece en tus sources")
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
	libChanged(w, i18n.Tf("«%s» añadida a la biblioteca", e.Title))
	s.render(w, "track_row", M{"Row": row})
}
