package api

import "regexp"

// Contenido +18. Algunas extensiones traen dos sources: el normal y su versión +18 (misma web, mismo idioma;
// p. ej. «Shadow Manga» y «Shadow Manga (+18)»). Se muestran como una sola tarjeta y la versión +18 solo se
// activa si se permite (en los ajustes de la extensión o, por defecto, en «Contenido +18») y la normal está activa.

var adultRe = regexp.MustCompile(`(?i)\+\s*18|18\s*\+|\bnsfw\b|\bhentai\b|\badult`)

func (s *Server) adultAllowed() bool { return s.Cfg.Adult == "sí" }

// adultFor: si la extensión de ese source (el normal) usa su versión +18. Lo que se elija en sus ajustes manda;
// si nunca se tocó, vale lo del ajuste general.
func (s *Server) adultFor(baseID string) bool {
	switch s.St.Get("adult.src."+baseID, "") {
	case "1":
		return true
	case "0":
		return false
	}
	return s.adultAllowed()
}

// adultOf: la versión +18 de un source normal ("" si no tiene).
func (s *Server) adultOf(baseID string) string {
	for a, b := range s.adultSiblings() {
		if b == baseID {
			return a
		}
	}
	return ""
}

// adultSiblings: source +18 -> su source normal (misma extensión e idioma).
func (s *Server) adultSiblings() map[string]string {
	type src struct{ id, pkg, name, lang string }
	var all []src
	for _, r := range s.rows(`SELECT id, pkg, name, lang FROM source`) {
		all = append(all, src{r["id"].(string), r["pkg"].(string), r["name"].(string), r["lang"].(string)})
	}
	out := map[string]string{}
	for _, a := range all {
		if !adultRe.MatchString(a.name) {
			continue
		}
		for _, b := range all {
			if b.pkg == a.pkg && b.lang == a.lang && b.id != a.id && !adultRe.MatchString(b.name) {
				out[a.id] = b.id
				break
			}
		}
	}
	return out
}

// ApplyAdult deja cada versión +18 activa solo si se permite y su normal está activa. Una que ya usa alguna
// serie no se desactiva (se quedaría sin capítulos nuevos).
func (s *Server) ApplyAdult() {
	for adult, base := range s.adultSiblings() {
		var baseOn, used int
		s.St.DB.QueryRow(`SELECT enabled FROM source WHERE id=?`, base).Scan(&baseOn)
		s.St.DB.QueryRow(`SELECT count(*) FROM series_source WHERE source_id=?`, adult).Scan(&used)
		on := s.adultFor(base) && baseOn == 1
		if used > 0 {
			on = true
		}
		s.St.DB.Exec(`UPDATE source SET enabled=? WHERE id=?`, b2i(on), adult)
	}
}
