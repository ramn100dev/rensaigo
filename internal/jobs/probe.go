package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"time"

	"rensaigo/internal/hostproc"
	"rensaigo/internal/i18n"
)

// chapterArg reconstruye el capítulo tal como lo dio la extensión (incluido su memo) para pedir sus páginas.
// Sample mide la calidad de un manga en un source sin descargarlo: cuántos capítulos tiene, el número del último y
// el ancho de una página de su último capítulo. Para comparar sources.
func (e *Engine) Sample(ctx context.Context, sourceID string, m Manga) (chapters int, last float64, width int, err error) {
	_, chs, err := e.details(ctx, sourceID, m.URL, m.Title, string(m.Memo))
	if err != nil {
		return 0, 0, 0, err
	}
	if len(chs) == 0 {
		return 0, 0, 0, nil
	}
	best := chs[0]
	for _, c := range chs {
		if n := chapterNumber(c); n > last {
			last, best = n, c
		}
	}
	res, err := e.SourceCall(ctx, sourceID, "pages", map[string]any{"chapter": chapterArg(best.URL, best.Name, best.Scanlator, chapterNumber(best), best.Date, string(best.Memo))})
	if err != nil {
		return len(chs), last, 0, nil // los capítulos sí se saben
	}
	var pr struct {
		Pages []map[string]any `json:"pages"`
	}
	json.Unmarshal(res, &pr)
	if len(pr.Pages) == 0 {
		return len(chs), last, 0, nil
	}
	pg := pr.Pages[len(pr.Pages)/2] // una del medio: la primera suele ser créditos o portada
	tmp := e.Cfg.Path("tmp", fmt.Sprintf("sample-%d.img", time.Now().UnixNano()))
	defer os.Remove(tmp)
	if _, err := e.SourceCall(ctx, sourceID, "image", map[string]any{"page": pg, "out": tmp}); err != nil {
		return len(chs), last, 0, nil
	}
	if f, err := os.Open(tmp); err == nil {
		if cfg, _, err := image.DecodeConfig(f); err == nil {
			width = cfg.Width
		}
		f.Close()
	}
	return len(chs), last, width, nil
}

// ProbeResult: resultado de la cadena completa contra un source, sin tocar la biblioteca
// (populares -> detalles y capítulos -> páginas -> una imagen). La usa el test de humo.
type ProbeResult struct {
	Source   string           `json:"source"`
	Step     string           `json:"failed_step,omitempty"` // primer paso que falló
	Kind     string           `json:"kind,omitempty"`        // clase del fallo
	Error    string           `json:"error,omitempty"`
	Mangas   int              `json:"mangas"`
	Title    string           `json:"title,omitempty"`
	Chapters int              `json:"chapters"`
	Pages    int              `json:"pages"`
	ImgBytes int64            `json:"image_bytes"`
	Ms       map[string]int64 `json:"ms"`
}

func (e *Engine) Probe(ctx context.Context, sourceID string) *ProbeResult {
	r := &ProbeResult{Source: sourceID, Ms: map[string]int64{}}
	fail := func(step string, err error) *ProbeResult {
		r.Step, r.Kind, r.Error = step, hostproc.Kind(err), trunc(err.Error(), 400)
		return r
	}
	timed := func(step string, f func() error) error {
		t := time.Now()
		err := f()
		r.Ms[step] = time.Since(t).Milliseconds()
		return err
	}
	var mangas []Manga
	if err := timed("popular", func() (err error) { mangas, err = e.Search(ctx, sourceID, ""); return }); err != nil {
		return fail("popular", err)
	}
	r.Mangas = len(mangas)
	if len(mangas) == 0 {
		r.Step, r.Kind, r.Error = "popular", "empty", i18n.T("la lista de populares llegó vacía")
		return r
	}
	var chs []Chapter
	// se prueban hasta 3 series: la primera puede no tener capítulos (licenciada, de pago…)
	for i := 0; i < len(mangas) && i < 3; i++ {
		m := mangas[i]
		r.Title = m.Title
		var err error
		if err = timed("details", func() (err error) { _, chs, err = e.details(ctx, sourceID, m.URL, m.Title, string(m.Memo)); return }); err != nil {
			return fail("details", err)
		}
		if len(chs) > 0 {
			break
		}
	}
	r.Chapters = len(chs)
	if len(chs) == 0 {
		r.Step, r.Kind, r.Error = "details", "empty", i18n.T("las series probadas no tienen capítulos")
		return r
	}
	ch := chs[len(chs)-1] // el más antiguo: suele ser gratuito
	var pr struct {
		Pages []map[string]any `json:"pages"`
	}
	if err := timed("pages", func() error {
		res, err := e.SourceCall(ctx, sourceID, "pages", map[string]any{"chapter": chapterArg(ch.URL, ch.Name, ch.Scanlator, ch.Number, ch.Date, string(ch.Memo))})
		if err == nil {
			json.Unmarshal(res, &pr)
		}
		return err
	}); err != nil {
		return fail("pages", err)
	}
	r.Pages = len(pr.Pages)
	if len(pr.Pages) == 0 {
		r.Step, r.Kind, r.Error = "pages", "empty", i18n.T("el capítulo no devolvió páginas")
		return r
	}
	out := filepath.Join(e.Cfg.Path("tmp"), "probe-"+sourceID+".img")
	defer os.Remove(out)
	if err := timed("image", func() error {
		res, err := e.SourceCall(ctx, sourceID, "image", map[string]any{"page": pr.Pages[0], "out": out})
		if err == nil {
			var ir struct {
				Bytes int64 `json:"bytes"`
			}
			json.Unmarshal(res, &ir)
			r.ImgBytes = ir.Bytes
		}
		return err
	}); err != nil {
		return fail("image", err)
	}
	return r
}
