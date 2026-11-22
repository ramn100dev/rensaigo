package api

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"net/http"
	"os"
	"sync"
	"time"

	"rensaigo/internal/library"

	"rensaigo/internal/jobs"
)

// candStat: al elegir otro source para una serie, cada fila pide aparte hasta qué capítulo llega ese source y el
// ancho de sus páginas (lo mismo que mide la cobertura). Se guarda media hora para no repetirlo al reabrir la lista.

type candResult struct {
	chapters int
	last     float64
	width    int
	err      string
	at       time.Time
}

var (
	candMu    sync.Mutex
	candCache = map[string]candResult{}
	candSem   = make(chan struct{}, 3) // como mucho 3 a la vez, para no ahogar al host de extensiones
)

func (s *Server) candStat(w http.ResponseWriter, r *http.Request) {
	id := pathInt(r, "id")
	res, ok := s.sample(r.Context(), r.FormValue("source"), jobs.Manga{URL: r.FormValue("url"), Title: r.FormValue("title"), Memo: json.RawMessage(r.FormValue("memo"))})
	if !ok {
		return
	}

	// lo que ya tienes, para comparar: el último capítulo conocido y el ancho del principal (si se midió)
	var mine float64
	s.St.DB.QueryRow(`SELECT coalesce(max(number),0) FROM chapter WHERE series_id=?`, id).Scan(&mine)
	lvl := "off"
	switch {
	case res.err != "" || res.chapters == 0:
	case mine == 0 || res.last >= mine:
		lvl = "ok"
	case res.last >= mine*0.8:
		lvl = "warn"
	default:
		lvl = "bad"
	}
	s.render(w, "cand_stat", M{"Err": res.err, "Chapters": res.chapters, "Last": res.last, "Width": res.width, "Mine": mine, "Lvl": lvl})
}

// sample mide un manga en un source (o da lo guardado). ok=false si la petición se canceló.
func (s *Server) sample(ctx context.Context, src string, m jobs.Manga) (candResult, bool) {
	key := src + "|" + m.URL
	candMu.Lock()
	res, ok := candCache[key]
	candMu.Unlock()
	if ok && time.Since(res.at) < 30*time.Minute {
		return res, true
	}
	select {
	case candSem <- struct{}{}:
	case <-ctx.Done():
		return res, false
	}
	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	n, last, wd, err := s.Eng.Sample(cctx, src, m)
	cancel()
	<-candSem
	if ctx.Err() != nil { // la página se cerró: no se guarda un resultado a medias
		return res, false
	}
	res = candResult{chapters: n, last: last, width: wd, at: time.Now()}
	if err != nil {
		res.err = err.Error()
	}
	candMu.Lock()
	candCache[key] = res
	candMu.Unlock()
	return res, true
}

// ---------------------------------------------------------------- ancho de página de un source ya vinculado

var (
	wMu    sync.Mutex
	wCache = map[string]int{} // cbz|mtime → ancho
)

// ssWidthDisk: el ancho de una página del medio del último capítulo descargado de ese source (0 si no hay).
func (s *Server) ssWidthDisk(ssID int64) int {
	var cbz string
	s.St.DB.QueryRow(`SELECT cbz FROM chapter WHERE series_source_id=? AND state='downloaded' AND cbz<>'' ORDER BY number DESC LIMIT 1`, ssID).Scan(&cbz)
	if cbz == "" {
		return 0
	}
	st, err := os.Stat(cbz)
	if err != nil {
		return 0
	}
	key := fmt.Sprintf("%s|%d", cbz, st.ModTime().UnixNano())
	wMu.Lock()
	wd, ok := wCache[key]
	wMu.Unlock()
	if ok {
		return wd
	}
	if c, err := library.OpenCBZ(cbz, false); err == nil {
		if len(c.Pages) > 0 {
			if rc, err := c.Pages[len(c.Pages)/2].Open(); err == nil {
				if cfg, _, err := image.DecodeConfig(rc); err == nil {
					wd = cfg.Width
				}
				rc.Close()
			}
		}
		c.Close()
	}
	wMu.Lock()
	wCache[key] = wd
	wMu.Unlock()
	return wd
}

// ssWidth: si de ese source aún no hay nada descargado, se mide en la web (como en «Añadir source»).
func (s *Server) ssWidth(w http.ResponseWriter, r *http.Request) {
	var src, u, title, memo string
	if s.St.DB.QueryRow(`SELECT source_id, url, coalesce(title,''), coalesce(memo,'') FROM series_source WHERE id=? AND series_id=?`,
		r.FormValue("ss"), pathInt(r, "id")).Scan(&src, &u, &title, &memo) != nil {
		return
	}
	res, ok := s.sample(r.Context(), src, jobs.Manga{URL: u, Title: title, Memo: json.RawMessage(memo)})
	if !ok || res.width == 0 {
		w.WriteHeader(http.StatusOK)
		return
	}
	fmt.Fprintf(w, `<span title="Ancho de página medido en la web"> · %d px</span>`, res.width)
}
