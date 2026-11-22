package api

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"rensaigo/internal/i18n"
	"rensaigo/internal/safe"
)

// Avisos fijos de la campana: problemas que siguen ahí hasta que se resuelven o se descartan (series que llevan
// mucho sin capítulos para su ritmo, sources que fallan desde hace horas, capítulos fallidos). No se guardan: se
// calculan al vuelo y se cachean un minuto. Descartar uno guarda su «firma»; si el problema cambia (sale un
// capítulo, el source se arregla y vuelve a fallar…) la firma cambia y el aviso vuelve.

type alert struct {
	Key, Sig string
	Level    string // warn | bad
	Text     string
	Href     string // enlace «ver»
	Act      string // acción rápida: check | retry (vacío = ninguna)
	ActLabel string
	at       int64 // para ordenar: más antiguo primero dentro de cada nivel
}

type alertCache struct {
	mu   sync.Mutex
	at   time.Time
	list []alert
}

const (
	cadYellow = 2.0 // veces su ritmo sin capítulos: aviso amarillo
	cadRed    = 5.0 // y rojo
	errYellow = 24 * time.Hour
	errRed    = 72 * time.Hour
)

func (s *Server) alerts() []alert {
	c := &s.alertC
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.at) < time.Minute && c.at.After(time.Time{}) {
		return c.list
	}
	all := s.computeAlerts()
	dis := map[string]string{}
	for _, r := range s.rows(`SELECT key, value FROM setting WHERE key LIKE 'dismiss:%'`) {
		dis[strings.TrimPrefix(r["key"].(string), "dismiss:")] = r["value"].(string)
	}
	var out []alert
	live := map[string]bool{}
	for _, a := range all {
		live[a.Key] = true
		if dis[a.Key] != a.Sig {
			out = append(out, a)
		}
	}
	for k := range dis { // descartes de problemas que ya no existen: fuera
		if !live[k] {
			s.St.DB.Exec(`DELETE FROM setting WHERE key=?`, "dismiss:"+k)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Level != out[j].Level {
			return out[i].Level == "bad"
		}
		return out[i].at < out[j].at
	})
	c.list, c.at = out, time.Now()
	return out
}

func (s *Server) dropAlertCache() {
	s.alertC.mu.Lock()
	s.alertC.at = time.Time{}
	s.alertC.mu.Unlock()
}

func (s *Server) computeAlerts() []alert {
	now := time.Now()
	var out []alert

	// --- sources que fallan: si fallan en todas sus series, un aviso por source; si no, uno por serie
	type ssErr struct {
		ssid, sid      int64
		src, srcName   string
		title, lastErr string
		since          int64
	}
	bySrc := map[string][]ssErr{}
	total := map[string]int{}
	rows, err := s.St.DB.Query(`SELECT ss.id, ss.series_id, ss.source_id, src.name, se.title, ss.last_error, ss.err_since
		FROM series_source ss JOIN source src ON src.id=ss.source_id JOIN series se ON se.id=ss.series_id
		WHERE src.enabled=1 AND se.paused=0`)
	if err == nil {
		for rows.Next() {
			var e ssErr
			rows.Scan(&e.ssid, &e.sid, &e.src, &e.srcName, &e.title, &e.lastErr, &e.since)
			total[e.src]++
			if e.since > 0 {
				bySrc[e.src] = append(bySrc[e.src], e)
			}
		}
		rows.Close()
	}
	level := func(since int64) string {
		switch d := now.Sub(time.Unix(since, 0)); {
		case d >= errRed:
			return "bad"
		case d >= errYellow:
			return "warn"
		}
		return ""
	}
	for src, errs := range bySrc {
		if len(errs) == total[src] && len(errs) > 1 {
			var since int64 // falla entero desde que cayó la última serie que le funcionaba
			for _, e := range errs {
				since = max(since, e.since)
			}
			if lv := level(since); lv != "" {
				out = append(out, alert{Key: "src:" + src, Sig: fmt.Sprint(since), Level: lv, at: since, Href: "/sources",
					Text: i18n.Tf("%s no responde en ninguna serie desde hace %s: %s", errs[0].srcName, span(now, since), friendlyErr(fmt.Errorf("%s", errs[0].lastErr)))})
			}
			continue
		}
		for _, e := range errs {
			if lv := level(e.since); lv != "" {
				out = append(out, alert{Key: fmt.Sprint("ss:", e.ssid), Sig: fmt.Sprint(e.since), Level: lv, at: e.since,
					Href: fmt.Sprint("/library/", e.sid), Act: "check", ActLabel: i18n.T("Reintentar"),
					Text: i18n.Tf("«%s»: %s falla desde hace %s: %s", e.title, e.srcName, span(now, e.since), friendlyErr(fmt.Errorf("%s", e.lastErr)))})
			}
		}
	}

	// --- capítulos fallidos
	for _, r := range s.rows(`SELECT c.series_id, se.title, count(*) n, max(c.id) mx, min(c.created) t FROM chapter c
		JOIN series se ON se.id=c.series_id WHERE c.state='failed' GROUP BY c.series_id`) {
		n := r["n"].(int64)
		txt := i18n.Tf("«%s»: %d capítulos no se pudieron descargar", r["title"], n)
		if n == 1 {
			txt = i18n.Tf("«%s»: un capítulo no se pudo descargar", r["title"])
		}
		out = append(out, alert{Key: fmt.Sprint("fail:", r["series_id"]), Sig: fmt.Sprint(r["mx"], ":", n), Level: "warn",
			at: r["t"].(int64), Href: fmt.Sprint("/library/", r["series_id"]), Act: "retry", ActLabel: i18n.T("Reintentar"), Text: txt})
	}

	// --- series paradas para su ritmo de publicación (semanal, quincenal, mensual), como Rensaio: hace falta un
	// ritmo claro (4 fechas al menos) y que la serie siga en publicación
	type ser struct {
		title string
		dates []int64
	}
	sers := map[int64]*ser{}
	rows, err = s.St.DB.Query(`SELECT se.id, se.title, c.date_upload FROM chapter c JOIN series se ON se.id=c.series_id
		WHERE se.paused=0 AND se.status IN (0,1) AND c.date_upload>0
		AND EXISTS(SELECT 1 FROM series_source ss JOIN source src ON src.id=ss.source_id WHERE ss.series_id=se.id AND src.enabled=1)
		ORDER BY se.id, c.date_upload DESC`)
	if err == nil {
		for rows.Next() {
			var id, d int64
			var t string
			rows.Scan(&id, &t, &d)
			x := sers[id]
			if x == nil {
				x = &ser{title: t}
				sers[id] = x
			}
			day := d / 86400 // varios capítulos el mismo día cuentan como una salida
			if len(x.dates) < 13 && (len(x.dates) == 0 || x.dates[len(x.dates)-1]/86400 != day) {
				x.dates = append(x.dates, d)
			}
		}
		rows.Close()
	}
	for id, x := range sers {
		cad := cadence(x.dates)
		if cad == 0 {
			continue
		}
		last := x.dates[0]
		days := now.Sub(time.Unix(last, 0)).Hours() / 24
		lv := ""
		switch {
		case days >= cad*cadRed:
			lv = "bad"
		case days >= cad*cadYellow:
			lv = "warn"
		}
		if lv == "" {
			continue
		}
		ritmo := map[float64]string{7: "semanal", 15: "quincenal", 30: "mensual"}[cad]
		out = append(out, alert{Key: fmt.Sprint("stale:", id), Sig: fmt.Sprint(last), Level: lv, at: last,
			Href: fmt.Sprint("/library/", id), Act: "check", ActLabel: i18n.T("Comprobar"),
			Text: i18n.Tf("«%s» suele salir %s y lleva %s sin capítulos", x.title, i18n.T(map[string]string{"semanal": "cada semana", "quincenal": "cada quince días", "mensual": "cada mes"}[ritmo]), span(now, last))})
	}
	return out
}

// cadence: ritmo de publicación en días (7, 15 o 30) a partir de las fechas de salida más recientes (de nueva a
// vieja, una por día), o 0 si no hay datos para fiarse o es más lento que mensual.
func cadence(dates []int64) float64 {
	if len(dates) < 4 {
		return 0
	}
	var iv []float64
	for i := 1; i < len(dates); i++ {
		iv = append(iv, float64(dates[i-1]-dates[i])/86400)
	}
	sort.Float64s(iv)
	med := iv[len(iv)/2]
	if len(iv)%2 == 0 {
		med = (iv[len(iv)/2-1] + iv[len(iv)/2]) / 2
	}
	switch {
	case med <= 10:
		return 7
	case med <= 22:
		return 15
	case med <= 45:
		return 30
	}
	return 0
}

// span: «3 días», «5 horas», «2 meses».
func span(now time.Time, since int64) string {
	d := now.Sub(time.Unix(since, 0))
	switch h := int(d.Hours()); {
	case h < 48:
		return i18n.Tf("%d horas", h)
	case h < 24*60:
		return i18n.Tf("%d días", h/24)
	default:
		return i18n.Tf("%d meses", h/24/30)
	}
}

// alertAction: acción rápida o descartar un aviso. Devuelve el panel de la campana entero.
func (s *Server) alertAction(w http.ResponseWriter, r *http.Request) {
	key := r.FormValue("k")
	var a *alert
	for _, x := range s.alerts() {
		if x.Key == key {
			a = &x
			break
		}
	}
	if a != nil {
		switch r.PathValue("act") {
		case "dismiss":
			s.St.Set("dismiss:"+a.Key, a.Sig)
		case "check":
			var sid int64
			fmt.Sscanf(a.Href, "/library/%d", &sid)
			safe.Go("comprobación", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
				defer cancel()
				s.Eng.CheckSeries(ctx, sid)
				s.dropAlertCache()
			})
			setToast(w, i18n.T("Comprobando…"))
		case "retry":
			var sid int64
			fmt.Sscanf(a.Href, "/library/%d", &sid)
			s.St.DB.Exec(`UPDATE chapter SET state='queued', attempts=0, next_try=0, error='' WHERE series_id=? AND state='failed'`, sid)
			s.Eng.Wake()
			setToast(w, i18n.T("Reintentando"))
		}
	}
	s.dropAlertCache()
	s.hdrEvents(w, r)
}
