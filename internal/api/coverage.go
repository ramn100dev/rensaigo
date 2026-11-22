package api

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"rensaigo/internal/jobs"
)

// Cobertura: qué tal le va a cada extensión con TU biblioteca. Se prueban una a una, en segundo plano y sin prisa:
// las que no están instaladas se instalan a prueba (firma verificada, sources desactivados), se buscan en ellas
// unas cuantas series de la biblioteca y se desinstalan. De lo encontrado se mira además cuántos capítulos tiene
// (frente a los que conocemos) y la resolución de una página. El resultado se guarda y ordena «Disponibles».

const (
	covSample = 12               // series de la biblioteca que se buscan en cada source
	covFresh  = 30 * 24 * 3600   // una medición vale un mes
	covPause  = 4 * time.Second  // respiro entre extensiones
)

type covState struct {
	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	lang    string
	cur     string // extensión que se está probando
	curPkg  string
	done    int
	total   int
}

type covTitle struct {
	title string
	last  float64 // último capítulo que conocemos
}

func (s *Server) covInit() {
	defer func() { // extensión que se estaba probando cuando se paró la app: fuera
		if p := s.St.Get("cov.trial", ""); p != "" {
			var on int
			s.St.DB.QueryRow(`SELECT count(*) FROM source WHERE pkg=? AND enabled=1`, p).Scan(&on)
			if on == 0 {
				s.Repo.UninstallTrial(p)
			}
			s.St.DB.Exec(`DELETE FROM setting WHERE key='cov.trial'`)
		}
	}()
	s.St.DB.Exec(`CREATE TABLE IF NOT EXISTS coverage(source_id TEXT PRIMARY KEY, pkg TEXT NOT NULL, name TEXT, lang TEXT,
		sampled INT NOT NULL DEFAULT 0, found INT NOT NULL DEFAULT 0, chap_ratio REAL NOT NULL DEFAULT 0, width INT NOT NULL DEFAULT 0,
		err TEXT NOT NULL DEFAULT '', at INT NOT NULL DEFAULT 0)`)
}

// libLang: idioma de la biblioteca (el más usado por los sources de las series), para no probar sources en
// idiomas que no lees.
func (s *Server) libLang() string {
	var l string
	s.St.DB.QueryRow(`SELECT src.lang FROM series_source ss JOIN source src ON src.id=ss.source_id
		GROUP BY src.lang ORDER BY count(*) DESC LIMIT 1`).Scan(&l)
	if i := strings.IndexByte(l, '-'); i > 0 {
		l = l[:i]
	}
	return l
}

func langMatch(srcLang, want string) bool {
	return srcLang == want || strings.HasPrefix(srcLang, want+"-")
}

func (s *Server) covSampleTitles() []covTitle {
	var out []covTitle
	// fija (no al azar) para que las mediciones de distintos días se puedan comparar
	for _, r := range s.rows(`SELECT s.title, coalesce(max(c.number),0) AS last FROM series s JOIN chapter c ON c.series_id=s.id
		GROUP BY s.id ORDER BY (s.id*2654435761)%1000 LIMIT ?`, covSample) {
		out = append(out, covTitle{r["title"].(string), r["last"].(float64)})
	}
	return out
}

func (s *Server) covStart(w http.ResponseWriter, r *http.Request) {
	c := &s.cov
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		s.covStatus(w, r)
		return
	}
	lang := r.FormValue("lang")
	if lang == "" || lang == "all" {
		lang = s.libLang()
	}
	if lang == "" {
		c.mu.Unlock()
		fail(w, 409, fmt.Errorf("elige un idioma en el filtro: la biblioteca aún no tiene sources"))
		return
	}
	titles := s.covSampleTitles()
	if len(titles) == 0 {
		c.mu.Unlock()
		fail(w, 409, fmt.Errorf("la biblioteca está vacía: no hay con qué comparar"))
		return
	}
	noJS := !s.jsInstalled()
	var pkgs []string
	// sin las +18 y primero las del idioma; después las multidioma (MangaDex, Comick…)
	for _, x := range s.rows(`SELECT r.pkg FROM repo_ext r WHERE (r.lang=? OR r.lang='all' OR r.lang LIKE ?) AND r.nsfw<>3
		AND NOT EXISTS(SELECT 1 FROM coverage v WHERE v.pkg=r.pkg AND v.at>? AND (v.lang=? OR v.lang LIKE ?))
		ORDER BY r.lang='all', r.name COLLATE NOCASE`, lang, lang+"-%", max(time.Now().Unix()-covFresh, s.libChangedAt()), lang, lang+"-%") {
		if p := x["pkg"].(string); !(noJS && needsJS(p, "")) {
			pkgs = append(pkgs, p)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.running, c.cancel, c.lang, c.done, c.total, c.cur = true, cancel, lang, 0, len(pkgs), ""
	c.mu.Unlock()
	go s.covRun(ctx, lang, pkgs, titles)
	s.covStatus(w, r)
}

func (s *Server) covStop(w http.ResponseWriter, r *http.Request) {
	s.cov.mu.Lock()
	if s.cov.cancel != nil {
		s.cov.cancel()
	}
	s.cov.mu.Unlock()
	s.covStatus(w, r)
}

func (s *Server) covStatus(w http.ResponseWriter, r *http.Request) {
	c := &s.cov
	c.mu.Lock()
	d := M{"Running": c.running, "Cur": c.cur, "Done": c.done, "Total": c.total, "Lang": c.lang}
	c.mu.Unlock()
	if !c.running && r.Header.Get("HX-Request") != "" && r.FormValue("poll") == "1" {
		w.Header().Set("HX-Refresh", "true") // acabó: se recarga la lista con los resultados
	}
	s.render(w, "cov_status", d)
}

func (s *Server) covRun(ctx context.Context, lang string, pkgs []string, titles []covTitle) {
	defer func() {
		s.cov.mu.Lock()
		s.cov.running, s.cov.cur, s.cov.curPkg = false, "", ""
		s.cov.mu.Unlock()
	}()
	tested := 0
	for i, pkg := range pkgs {
		if ctx.Err() != nil {
			return
		}
		var name string
		s.St.DB.QueryRow(`SELECT name FROM repo_ext WHERE pkg=?`, pkg).Scan(&name)
		s.cov.mu.Lock()
		s.cov.cur, s.cov.curPkg, s.cov.done = name, pkg, i
		s.cov.mu.Unlock()
		s.covPackage(ctx, pkg, lang, titles)
		tested++
		// el host acumula las clases de cada extensión probada: de vez en cuando se reinicia, si no está bajando nada
		if tested%8 == 0 && len(s.Eng.Snapshot().Active) == 0 {
			s.Host.Stop()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(covPause):
		}
	}
	s.cov.mu.Lock()
	s.cov.done = len(pkgs)
	s.cov.mu.Unlock()
	if len(s.Eng.Snapshot().Active) == 0 {
		s.Host.Stop()
	}
}

func (s *Server) covPackage(ctx context.Context, pkg, lang string, titles []covTitle) {
	type src struct{ id, name, lang string }
	var srcs []src
	installed := false
	var n int
	s.St.DB.QueryRow(`SELECT count(*) FROM ext WHERE pkg=?`, pkg).Scan(&n)
	if n > 0 {
		installed = true
	} else {
		s.St.Set("cov.trial", pkg) // si se corta a medias (reinicio), se quita al arrancar
		ictx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		infos, err := s.Repo.InstallTrial(ictx, pkg)
		cancel()
		if err != nil {
			s.St.DB.Exec(`INSERT OR REPLACE INTO coverage(source_id,pkg,name,lang,err,at) VALUES(?,?,?,?,?,?)`,
				"pkg:"+pkg, pkg, pkg, lang, trunc(err.Error(), 200), time.Now().Unix())
			return
		}
		defer func() {
			// solo si nadie la ha empezado a usar mientras tanto (Uninstall se niega si alguna serie la usa)
			var on int
			s.St.DB.QueryRow(`SELECT count(*) FROM source WHERE pkg=? AND enabled=1`, pkg).Scan(&on)
			if on == 0 {
				if err := s.Repo.UninstallTrial(pkg); err != nil {
					log.Printf("cobertura: no se pudo quitar %s: %v", pkg, err)
				}
			}
			s.St.DB.Exec(`DELETE FROM setting WHERE key='cov.trial'`)
		}()
		_ = infos
	}
	for _, x := range s.rows(`SELECT id, name, lang FROM source WHERE pkg=?`, pkg) {
		if langMatch(x["lang"].(string), lang) {
			srcs = append(srcs, src{x["id"].(string), x["name"].(string), x["lang"].(string)})
		}
	}
	_ = installed
	for _, sr := range srcs {
		if ctx.Err() != nil {
			return
		}
		s.covSource(ctx, pkg, sr.id, sr.name, sr.lang, titles)
	}
}

func (s *Server) covSource(ctx context.Context, pkg, id, name, lang string, titles []covTitle) {
	found, sampled, errs := 0, 0, 0
	var lastErr error
	type hit struct {
		m    jobs.Manga
		last float64
	}
	var hits []hit
	for _, t := range titles {
		if ctx.Err() != nil {
			return
		}
		c, cancel := context.WithTimeout(ctx, 60*time.Second)
		ms, err := s.Eng.Search(c, id, t.title)
		cancel()
		if err != nil {
			errs++
			lastErr = err
			if sampled == 0 && errs >= 3 { // no responde: no vale la pena seguir
				break
			}
			continue
		}
		sampled++
		best, score := -1, 0.0
		for i, m := range ms {
			if sc := titleScore(t.title, m.Title); sc > score {
				best, score = i, sc
			}
		}
		if best >= 0 && score >= 0.85 {
			found++
			hits = append(hits, hit{ms[best], t.last})
		}
	}
	// calidad: de las dos primeras que tiene, capítulos frente a los que conocemos y ancho de una página
	var ratios []float64
	width := 0
	for i, h := range hits {
		if i >= 2 || ctx.Err() != nil {
			break
		}
		c, cancel := context.WithTimeout(ctx, 2*time.Minute)
		_, last, wd, err := s.Eng.Sample(c, id, h.m)
		cancel()
		if err != nil {
			continue
		}
		if h.last > 0 && last > 0 { // sin números de capítulo no se puede comparar
			ratios = append(ratios, min(last/h.last, 1.5))
		}
		width = max(width, wd)
	}
	ratio := 0.0
	if len(ratios) > 0 {
		sort.Float64s(ratios)
		ratio = ratios[0]
	}
	msg := ""
	if sampled == 0 && lastErr != nil {
		msg = friendlyErr(lastErr)
	}
	s.St.DB.Exec(`DELETE FROM coverage WHERE source_id=?`, "pkg:"+pkg)
	s.St.DB.Exec(`INSERT OR REPLACE INTO coverage(source_id,pkg,name,lang,sampled,found,chap_ratio,width,err,at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		id, pkg, name, lang, sampled, found, ratio, width, trunc(msg, 200), time.Now().Unix())
}

func (s *Server) covPkg() string {
	s.cov.mu.Lock()
	defer s.cov.mu.Unlock()
	return s.cov.curPkg
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// covFor: la mejor medición de cada extensión en un idioma (pkg -> datos para la tarjeta).
func (s *Server) covFor(lang string) map[string]M {
	out := map[string]M{}
	changed := s.libChangedAt()
	q := `SELECT pkg, name, lang, sampled, found, chap_ratio, width, err, at FROM coverage`
	var args []any
	if lang != "" && lang != "all" {
		q += ` WHERE lang=? OR lang LIKE ?`
		args = append(args, lang, lang+"-%")
	}
	for _, r := range s.rows(q+` ORDER BY found DESC, width DESC`, args...) {
		p := r["pkg"].(string)
		if _, ok := out[p]; ok {
			continue
		}
		sampled, found := r["sampled"].(int64), r["found"].(int64)
		pct := int64(0)
		if sampled > 0 {
			pct = found * 100 / sampled
		}
		r["pct"] = pct
		r["score"] = covScore(pct, r["width"].(int64), r["chap_ratio"].(float64))
		lvl := "bad"
		switch {
		case r["err"] != "" || sampled == 0:
			lvl = "off"
		case pct >= 60:
			lvl = "ok"
		case pct >= 30:
			lvl = "warn"
		}
		r["lvl"] = lvl
		r["stale"] = r["at"].(int64) < changed // se midió antes de añadir series
		out[p] = r
	}
	return out
}

// covScore: para ordenar. Pesa sobre todo tener tus series; luego que estén completas y se vean bien.
func covScore(pct, width int64, ratio float64) float64 {
	q := 0.5
	if width > 0 {
		q = min(float64(width)/1200, 1)
	}
	return float64(pct) * (0.6 + 0.25*min(ratio, 1) + 0.15*q)
}

// libChangedAt: cuándo se añadió la última serie a la biblioteca (lo medido antes de eso está desactualizado).
func (s *Server) libChangedAt() int64 {
	var t int64
	s.St.DB.QueryRow(`SELECT coalesce(max(created),0) FROM series`).Scan(&t)
	return t
}

// CoverageLoop vuelve a medir solas las extensiones INSTALADAS cuando la biblioteca cambia (series nuevas), un par
// de minutos después del último cambio para no repetirlo por cada serie añadida. Las no instaladas solo se marcan
// como desactualizadas (hay que instalarlas a prueba): esas se repiten con el botón de escanear.
func (s *Server) CoverageLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		changed := s.libChangedAt()
		if changed == 0 || time.Now().Unix()-changed < 120 {
			continue
		}
		s.cov.mu.Lock()
		busy := s.cov.running
		s.cov.mu.Unlock()
		if busy {
			continue
		}
		lang := s.libLang()
		titles := s.covSampleTitles()
		if lang == "" || len(titles) == 0 {
			continue
		}
		var pkgs []string
		seen := map[string]bool{}
		for _, x := range s.rows(`SELECT s.pkg, s.lang FROM source s JOIN ext e ON e.pkg=s.pkg WHERE s.enabled=1
			AND NOT EXISTS(SELECT 1 FROM coverage v WHERE v.source_id=s.id AND v.at>=?) ORDER BY s.name`, changed) {
			p := x["pkg"].(string)
			if !seen[p] && langMatch(x["lang"].(string), lang) {
				seen[p] = true
				pkgs = append(pkgs, p)
			}
		}
		if len(pkgs) == 0 {
			continue
		}
		log.Printf("cobertura: la biblioteca cambió, se vuelven a medir %d extensiones instaladas", len(pkgs))
		cctx, cancel := context.WithCancel(ctx)
		s.cov.mu.Lock()
		s.cov.running, s.cov.cancel, s.cov.lang, s.cov.done, s.cov.total, s.cov.cur = true, cancel, lang, 0, len(pkgs), ""
		s.cov.mu.Unlock()
		s.covRun(cctx, lang, pkgs, titles)
		cancel()
	}
}
