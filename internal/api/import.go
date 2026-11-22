package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"rensaigo/internal/i18n"
	"rensaigo/internal/library"
)

// Importar: emparejar con un source las series adoptadas del disco (de Rensaio o de una carpeta cualquiera).

// importData: datos de la sección "Importar biblioteca", que ahora forma parte de Sistema.
func (s *Server) importData() M {
	pending := s.rows(`SELECT s.id, s.title, s.category, s.hint_source,
		(SELECT count(*) FROM chapter WHERE series_id=s.id AND state='downloaded') AS chapters,
		'all' AS suggested
		FROM series s WHERE s.imported=1 AND NOT EXISTS(SELECT 1 FROM series_source WHERE series_id=s.id) ORDER BY s.title`)
	var rep *library.Report
	if j := s.St.Get("last_import", ""); j != "" {
		rep = &library.Report{}
		json.Unmarshal([]byte(j), rep)
	}
	return M{"Pending": pending, "Report": rep, "Sources": s.enabledSources(), "Lib": s.Cfg.LibraryDir}
}

func (s *Server) importIndex(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/system#importar", http.StatusSeeOther)
}

func (s *Server) importScan(w http.ResponseWriter, r *http.Request) {
	rep := s.QuickScan(true)
	if rep == nil {
		fail(w, 500, i18n.Errorf("no se pudo escanear %s", s.Cfg.LibraryDir))
		return
	}
	w.Header().Set("HX-Redirect", "/system#importar")
	if r.Header.Get("HX-Request") == "" {
		json.NewEncoder(w).Encode(rep)
	}
}

func (s *Server) importCandidates(w http.ResponseWriter, r *http.Request) {
	id := pathInt(r, "id")
	q := r.FormValue("q")
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	ms, err := s.Eng.Search(ctx, r.FormValue("source"), q)
	if len(ms) > 12 {
		ms = ms[:12]
	}
	data := M{"ID": id, "Source": r.FormValue("source"), "Mangas": ms, "Q": q, "Sources": s.enabledSources()}
	if err != nil {
		data["Err"] = friendlyErr(err)
	}
	tpl := "import_modal"
	if r.FormValue("partial") == "1" {
		tpl = "import_candidates"
	}
	s.render(w, tpl, data)
}

// importMatch: el usuario confirma qué serie del source corresponde. Se enlaza y se comprueba; los capítulos
// ya presentes en disco se reconocen por número y NO se vuelven a descargar.
func (s *Server) importMatch(w http.ResponseWriter, r *http.Request) {
	id := pathInt(r, "id")
	if err := s.Eng.AddSource(id, r.FormValue("source"), r.FormValue("url"), r.FormValue("title"), r.FormValue("memo")); err != nil {
		fail(w, 500, err)
		return
	}
	s.St.DB.Exec(`UPDATE series SET paused=0, auto_download=? WHERE id=?`, b2i(r.FormValue("auto") == "1"), id)
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	if err := s.Eng.CheckSeries(ctx, id); err != nil {
		fail(w, 502, err)
		return
	}
	w.Header().Set("HX-Redirect", fmt.Sprintf("/library/%d", id))
}

// importAuto empareja una serie importada sin abrir diálogos:
//   - con un source concreto busca el título y, si hay un resultado claramente igual, lo enlaza;
//   - con «all» busca en todos los sources activos y devuelve la fila con los candidatos para que el usuario elija;
//   - con url (candidato elegido) lo enlaza directamente.
//
// Al enlazar se comprueba la serie y se deja la descarga automática activada; la fila desaparece de la tabla.
func (s *Server) importAuto(w http.ResponseWriter, r *http.Request) {
	id := pathInt(r, "id")
	src := r.FormValue("source")
	var title string
	s.St.DB.QueryRow(`SELECT title FROM series WHERE id=?`, id).Scan(&title)
	srcName := func(id string) (n string) {
		s.St.DB.QueryRow(`SELECT name FROM source WHERE id=?`, id).Scan(&n)
		return
	}
	row := func(extra M) {
		d := s.importData()
		for _, p := range d["Pending"].([]M) {
			if p["id"].(int64) == id {
				p["suggested"] = src
				for k, v := range extra {
					p[k] = v
				}
				s.render(w, "pair_row", M{"P": p, "Sources": d["Sources"]})
				return
			}
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Minute)
	defer cancel()
	link := func(src, url, mtitle, memo string) {
		if err := s.Eng.AddSource(id, src, url, mtitle, memo); err != nil {
			row(M{"msg": err.Error()})
			return
		}
		s.St.DB.Exec(`UPDATE series SET paused=0, auto_download=1 WHERE id=?`, id)
		if err := s.Eng.CheckSeries(ctx, id); err != nil {
			s.St.Event("importación", "«%s» enlazada con %s, pero la comprobación falló: %v", title, srcName(src), err)
		}
		setToast(w, i18n.Tf("«%s» emparejada con %s", title, srcName(src)))
	}

	if url := r.FormValue("url"); url != "" {
		link(src, url, r.FormValue("title"), r.FormValue("memo"))
		return
	}

	if src == "all" {
		cands, fails := s.searchAll(ctx, []string{title})
		if len(cands) > 12 {
			cands = cands[:12]
		}
		extra := M{"cands": cands}
		if len(cands) == 0 {
			extra["msg"] = i18n.T("No aparece en ningún source activo")
		}
		if len(fails) > 0 {
			extra["fails"] = strings.Join(fails, ", ")
		}
		row(extra)
		return
	}

	ms, err := s.Eng.Search(ctx, src, title)
	if err != nil {
		row(M{"msg": friendlyErr(err)})
		return
	}
	best, score := -1, 0.0
	for i, m := range ms {
		if sc := titleScore(title, m.Title); sc > score {
			best, score = i, sc
		}
	}
	if best < 0 || score < 0.8 {
		msg := i18n.Tf("No aparece en %s", srcName(src))
		if best >= 0 {
			msg += " " + i18n.Tf("(lo más parecido: «%s»)", ms[best].Title)
		}
		row(M{"msg": msg})
		return
	}
	m := ms[best]
	link(src, m.URL, m.Title, string(m.Memo))
}

// titleScore compara dos títulos sin mayúsculas, tildes ni signos: 1 si son iguales, si no el coeficiente de Dice
// de sus bigramas.
func titleScore(a, b string) float64 {
	a, b = normTitle(a), normTitle(b)
	if a == "" || b == "" {
		return 0
	}
	if a == b {
		return 1
	}
	bg := func(x string) map[string]int {
		m := map[string]int{}
		rs := []rune(x)
		for i := 0; i+1 < len(rs); i++ {
			m[string(rs[i:i+2])]++
		}
		return m
	}
	ma, mb := bg(a), bg(b)
	var inter, n int
	for k, v := range ma {
		n += v
		if w := mb[k]; w > 0 {
			inter += min(v, w)
		}
	}
	for _, v := range mb {
		n += v
	}
	if n == 0 {
		return 0
	}
	return 2 * float64(inter) / float64(n)
}

func normTitle(t string) string {
	rep := strings.NewReplacer("á", "a", "é", "e", "í", "i", "ó", "o", "ú", "u", "ü", "u", "ñ", "n", "à", "a", "è", "e", "ò", "o")
	t = rep.Replace(strings.ToLower(t))
	var b strings.Builder
	for _, r := range t {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r > 127 {
			b.WriteRune(r)
		}
	}
	return b.String()
}
