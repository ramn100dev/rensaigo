package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"rensaigo/internal/config"
	"rensaigo/internal/library"
	"rensaigo/internal/store"
)

// fakeHost hace de host JVM: cada source tiene sus mangas (por URL) con sus capítulos, y las imágenes son PNG
// diminutos. Apunta el orden en que se piden las páginas de cada capítulo.
type fakeHost struct {
	mu       sync.Mutex
	chapters map[string]map[string][]Chapter // source → url del manga → capítulos
	pages    int
	failOp   map[string]error
	block    chan struct{} // si no es nil, las imágenes esperan a que se cierre (o a que se cancele)
	order    []string      // "source:capítulo" en el orden en que se empezó a descargar
}

func newFakeHost() *fakeHost {
	return &fakeHost{chapters: map[string]map[string][]Chapter{}, pages: 3, failOp: map[string]error{}}
}

func (f *fakeHost) set(src, url string, nums ...float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.chapters[src] == nil {
		f.chapters[src] = map[string][]Chapter{}
	}
	var cs []Chapter
	for _, n := range nums {
		cs = append(cs, Chapter{URL: fmt.Sprintf("%s/%s/%g", src, url, n), Name: fmt.Sprintf("Capítulo %g", n), Number: n})
	}
	f.chapters[src][url] = cs
}

var pngBytes = func() []byte {
	var b bytes.Buffer
	png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 8, 12)))
	return b.Bytes()
}()

func (f *fakeHost) CallJar(ctx context.Context, jar, op string, args map[string]any) (json.RawMessage, error) {
	f.mu.Lock()
	err := f.failOp[op]
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	src := args["source"].(string)
	switch op {
	case "search", "popular":
		return json.Marshal(M{"mangas": []Manga{{URL: "/m", Title: "Encontrada"}}})
	case "details":
		m := args["manga"].(map[string]any)
		f.mu.Lock()
		cs := f.chapters[src][m["url"].(string)]
		f.mu.Unlock()
		return json.Marshal(M{"manga": M{"url": m["url"], "title": m["title"], "author": "Autora", "genre": "Acción"}, "chapters": cs})
	case "pages":
		c := args["chapter"].(map[string]any)
		f.mu.Lock()
		f.order = append(f.order, src+":"+c["url"].(string))
		n := f.pages
		f.mu.Unlock()
		var ps []M
		for i := 0; i < n; i++ {
			ps = append(ps, M{"index": i, "imageUrl": fmt.Sprintf("https://x/%d", i)})
		}
		return json.Marshal(M{"pages": ps})
	case "image":
		f.mu.Lock()
		b := f.block
		f.mu.Unlock()
		if b != nil {
			select {
			case <-b:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return json.RawMessage(`{}`), os.WriteFile(args["out"].(string), pngBytes, 0o644)
	}
	return nil, fmt.Errorf("op desconocida %s", op)
}

type M = map[string]any

type fakeJars struct{}

func (fakeJars) JarFor(string) (string, error) { return "fake.jar", nil }

func newTestEngine(t *testing.T) (*Engine, *fakeHost) {
	t.Helper()
	tmp := t.TempDir()
	cfg := &config.Config{ConfigDir: filepath.Join(tmp, "config"), LibraryDir: filepath.Join(tmp, "lib"), DefaultCat: "Manga",
		DLConcurrency: 3, PerSource: 1, PageConcurrency: 2, MaxAttempts: 3, CheckInterval: time.Hour}
	os.MkdirAll(cfg.Path("tmp"), 0o755)
	st, err := store.Open(cfg.Path("rensaigo.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for _, id := range []string{"A", "B", "C"} {
		if _, err := st.DB.Exec(`INSERT INTO source(id,pkg,name,lang,base_url,enabled) VALUES(?,?,?,?,?,1)`, id, "pkg."+id, "Src"+id, "es", "https://"+id); err != nil {
			t.Fatal(err)
		}
	}
	h := newFakeHost()
	return New(cfg, st, h, fakeJars{}), h
}

// drain lanza descargas hasta que no queda nada pendiente (o se agota el tiempo).
func drain(t *testing.T, e *Engine) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		e.dispatch(ctx)
		waitIdle(t, e)
		var n int
		e.St.DB.QueryRow(`SELECT count(*) FROM chapter WHERE (state='queued' OR (state='downloaded' AND upg=1)) AND next_try<=?`, time.Now().Unix()).Scan(&n)
		if n == 0 {
			return
		}
	}
	t.Fatal("la cola no se vació")
}

func waitIdle(t *testing.T, e *Engine) {
	t.Helper()
	for i := 0; i < 500; i++ {
		e.mu.Lock()
		n := len(e.active)
		e.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("descargas colgadas")
}

func count(e *Engine, q string, args ...any) int {
	var n int
	e.St.DB.QueryRow(q, args...).Scan(&n)
	return n
}

func TestFollowAndDownload(t *testing.T) {
	e, h := newTestEngine(t)
	h.set("A", "/m", 1, 2, 2.5)
	var got []float64
	e.OnDownloaded = func(_ int64, _ string, n float64) { got = append(got, n) }
	sid, err := e.Follow(context.Background(), "A", Manga{URL: "/m", Title: "Mi Serie"}, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if n := count(e, `SELECT count(*) FROM chapter WHERE series_id=? AND state='queued'`, sid); n != 3 {
		t.Fatalf("en cola: %d, esperado 3", n)
	}
	var author string
	e.St.DB.QueryRow(`SELECT author FROM series WHERE id=?`, sid).Scan(&author)
	if author != "Autora" {
		t.Errorf("metadatos sin completar: autor %q", author)
	}
	drain(t, e)
	rows, _ := e.St.DB.Query(`SELECT number, cbz, pages FROM chapter WHERE series_id=? ORDER BY number`, sid)
	defer rows.Close()
	for rows.Next() {
		var n float64
		var cbz string
		var pages int
		rows.Scan(&n, &cbz, &pages)
		c, err := library.OpenCBZ(cbz, true)
		if err != nil {
			t.Fatalf("capítulo %g: %v", n, err)
		}
		if len(c.Pages) != 3 || pages != 3 || c.Info == nil || c.Info.Series != "Mi Serie" {
			t.Errorf("capítulo %g: %d páginas (fila %d), info %+v", n, len(c.Pages), pages, c.Info)
		}
		c.Close()
		if !strings.HasPrefix(cbz, filepath.Join(e.Cfg.LibraryDir, "Manga", "Mi Serie")) {
			t.Errorf("CBZ fuera de su carpeta: %s", cbz)
		}
	}
	if len(got) != 3 {
		t.Errorf("avisos de capítulo nuevo: %v", got)
	}
	// volver a comprobar no duplica nada
	if err := e.CheckSeries(context.Background(), sid); err != nil {
		t.Fatal(err)
	}
	if n := count(e, `SELECT count(*) FROM chapter WHERE series_id=?`, sid); n != 3 {
		t.Errorf("capítulos tras volver a comprobar: %d", n)
	}
}

func TestNoAutoAndFromChapter(t *testing.T) {
	e, h := newTestEngine(t)
	h.set("A", "/m", 1, 2, 3, 4)
	sid, _ := e.Follow(context.Background(), "A", Manga{URL: "/m", Title: "S"}, "", false)
	if n := count(e, `SELECT count(*) FROM chapter WHERE state='available'`); n != 4 {
		t.Fatalf("sin descarga automática deberían quedar 4 disponibles: %d", n)
	}
	e.St.DB.Exec(`DELETE FROM chapter`)
	e.St.DB.Exec(`UPDATE series_source SET from_ch=3 WHERE series_id=?`, sid)
	e.CheckSeries(context.Background(), sid)
	if n := count(e, `SELECT count(*) FROM chapter WHERE number<3`); n != 0 {
		t.Errorf("«desde el 3» y entran %d anteriores", n)
	}
	if n := count(e, `SELECT count(*) FROM chapter`); n != 2 {
		t.Errorf("capítulos desde el 3: %d", n)
	}
}

// Dos series del mismo source: se van turnando, no se baja una entera antes de empezar la otra.
func TestDispatchFairness(t *testing.T) {
	e, h := newTestEngine(t)
	e.Cfg.DLConcurrency = 1
	h.set("A", "/x", 1, 2, 3)
	h.set("A", "/y", 1, 2, 3)
	e.Follow(context.Background(), "A", Manga{URL: "/x", Title: "X"}, "", true)
	e.Follow(context.Background(), "A", Manga{URL: "/y", Title: "Y"}, "", true)
	drain(t, e)
	var series []string
	for _, o := range h.order {
		series = append(series, strings.Split(o, "/")[2]) // "A:A//x/1"
	}
	want := "x y x y x y"
	if got := strings.Join(series, " "); got != want {
		t.Errorf("orden de descarga %q, esperado %q", got, want)
	}
}

// El capítulo que solo tenía el source temporal se baja de él; cuando sale en el principal, se sustituye y el
// archivo viejo desaparece.
func TestSubstitution(t *testing.T) {
	e, h := newTestEngine(t)
	h.set("A", "/p", 1, 2)
	h.set("B", "/t", 1, 2, 3)
	sid, _ := e.Follow(context.Background(), "A", Manga{URL: "/p", Title: "S"}, "", true)
	if err := e.AddSource(sid, "B", "/t", "S", ""); err != nil {
		t.Fatal(err)
	}
	e.CheckSeries(context.Background(), sid)
	drain(t, e)
	var src, old string
	var temp int
	e.St.DB.QueryRow(`SELECT source_name, cbz, temp FROM chapter WHERE number=3`).Scan(&src, &old, &temp)
	if src != "SrcB" || temp != 1 {
		t.Fatalf("capítulo 3: de %q, temp=%d (esperado SrcB, temporal)", src, temp)
	}
	if n := count(e, `SELECT count(*) FROM chapter WHERE number<3 AND source_name='SrcA'`); n != 2 {
		t.Errorf("1 y 2 deberían venir del principal: %d", n)
	}
	h.set("A", "/p", 1, 2, 3)
	e.CheckSeries(context.Background(), sid)
	if n := count(e, `SELECT count(*) FROM chapter WHERE number=3 AND upg=1 AND state='downloaded'`); n != 1 {
		t.Fatal("el 3 debería quedar pendiente de sustituir (y seguir legible)")
	}
	drain(t, e)
	var cbz string
	e.St.DB.QueryRow(`SELECT source_name, cbz, temp FROM chapter WHERE number=3`).Scan(&src, &cbz, &temp)
	if src != "SrcA" || temp != 0 {
		t.Errorf("tras sustituir: de %q, temp=%d", src, temp)
	}
	if _, err := os.Stat(cbz); err != nil {
		t.Errorf("archivo nuevo: %v", err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("el archivo del temporal sigue ahí: %s", old)
	}
	// reordenar: B pasa a principal y lo de A a temporal
	var ssA, ssB int64
	e.St.DB.QueryRow(`SELECT id FROM series_source WHERE source_id='A'`).Scan(&ssA)
	e.St.DB.QueryRow(`SELECT id FROM series_source WHERE source_id='B'`).Scan(&ssB)
	e.SetOrder(sid, []int64{ssB, ssA})
	if n := count(e, `SELECT count(*) FROM chapter WHERE source_name='SrcA' AND temp=1`); n != 3 {
		t.Errorf("tras reordenar, lo de A debería ser temporal: %d", n)
	}
}

func TestRetryThenFailed(t *testing.T) {
	e, h := newTestEngine(t)
	h.set("A", "/m", 1)
	e.Follow(context.Background(), "A", Manga{URL: "/m", Title: "S"}, "", true)
	h.failOp["pages"] = errors.New("HTTP error 503")
	var id int64
	e.St.DB.QueryRow(`SELECT id FROM chapter`).Scan(&id)
	for i := 1; i <= e.Cfg.MaxAttempts; i++ {
		e.St.DB.Exec(`UPDATE chapter SET next_try=0 WHERE id=?`, id)
		e.dispatch(context.Background())
		waitIdle(t, e)
		var state string
		var attempts int
		var next int64
		e.St.DB.QueryRow(`SELECT state, attempts, next_try FROM chapter WHERE id=?`, id).Scan(&state, &attempts, &next)
		if attempts != i {
			t.Fatalf("intento %d: attempts=%d", i, attempts)
		}
		if i < e.Cfg.MaxAttempts && (state != "queued" || next < time.Now().Unix()+20) { // espera creciente, no 3 s
			t.Fatalf("intento %d: estado %s, reintento en %d", i, state, next-time.Now().Unix())
		}
		if i == e.Cfg.MaxAttempts && state != "failed" {
			t.Fatalf("tras %d intentos: %s", i, state)
		}
	}
	// reintentar a mano lo devuelve a la cola desde cero
	delete(h.failOp, "pages")
	e.Queue(0, nil)
	var sid int64
	e.St.DB.QueryRow(`SELECT series_id FROM chapter WHERE id=?`, id).Scan(&sid)
	if n, _ := e.Queue(sid, nil); n != 1 {
		t.Fatal("no se pudo volver a encolar el fallido")
	}
	drain(t, e)
	if n := count(e, `SELECT count(*) FROM chapter WHERE state='downloaded' AND attempts=0`); n != 1 {
		t.Error("el reintento no acabó descargado")
	}
}

// Una sustitución que falla nunca pasa a «fallido»: el archivo de antes sigue descargado.
func TestSubstitutionNeverFails(t *testing.T) {
	e, _ := newTestEngine(t)
	e.St.DB.Exec(`INSERT INTO series(title,category,path,created) VALUES('S','Manga','/x',0)`)
	e.St.DB.Exec(`INSERT INTO chapter(series_id,number,state,cbz,upg,created) VALUES(1,1,'downloaded','/x/a.cbz',1,0)`)
	for i := 0; i < e.Cfg.MaxAttempts+2; i++ {
		e.fail(1, errors.New("roto"))
	}
	var state string
	var upg int
	e.St.DB.QueryRow(`SELECT state, upg FROM chapter WHERE id=1`).Scan(&state, &upg)
	if state != "downloaded" || upg != 1 {
		t.Errorf("sustitución fallida: estado %s, upg %d", state, upg)
	}
}

// Borrar la serie con un capítulo a medias: la descarga se para y no vuelve a crear la carpeta.
func TestCancelSeriesDuringDownload(t *testing.T) {
	e, h := newTestEngine(t)
	h.set("A", "/m", 1)
	h.block = make(chan struct{})
	sid, _ := e.Follow(context.Background(), "A", Manga{URL: "/m", Title: "Borrada"}, "", true)
	e.dispatch(context.Background())
	e.mu.Lock()
	n := len(e.active)
	e.mu.Unlock()
	if n != 1 {
		t.Fatal("la descarga no empezó")
	}
	e.CancelSeries(sid)
	e.St.DB.Exec(`DELETE FROM series WHERE id=?`, sid)
	close(h.block)
	waitIdle(t, e)
	if _, err := os.Stat(filepath.Join(e.Cfg.LibraryDir, "Manga", "Borrada")); !os.IsNotExist(err) {
		t.Error("la carpeta de la serie borrada ha vuelto a aparecer")
	}
	if n := count(e, `SELECT count(*) FROM chapter`); n != 0 {
		t.Errorf("quedan %d capítulos", n)
	}
}

// Si el capítulo desaparece de la BD durante la descarga (source quitado), no se escribe nada.
func TestChapterGoneDuringDownload(t *testing.T) {
	e, h := newTestEngine(t)
	h.set("A", "/m", 1)
	h.block = make(chan struct{})
	e.Follow(context.Background(), "A", Manga{URL: "/m", Title: "S"}, "", true)
	e.dispatch(context.Background())
	e.St.DB.Exec(`DELETE FROM chapter`)
	close(h.block)
	waitIdle(t, e)
	matches, _ := filepath.Glob(filepath.Join(e.Cfg.LibraryDir, "*", "*", "*.cbz"))
	if len(matches) != 0 {
		t.Errorf("se escribió %v", matches)
	}
}

func TestRecoverAndPause(t *testing.T) {
	e, h := newTestEngine(t)
	h.set("A", "/m", 1, 2)
	sid, _ := e.Follow(context.Background(), "A", Manga{URL: "/m", Title: "S"}, "", true)
	e.St.DB.Exec(`UPDATE chapter SET state='downloading' WHERE number=1`)
	dir := filepath.Join(e.Cfg.LibraryDir, "Manga", "S")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "x.cbz.part"), []byte("a medias"), 0o644)
	e.Recover()
	if n := count(e, `SELECT count(*) FROM chapter WHERE state='queued'`); n != 2 {
		t.Errorf("tras recuperar, en cola: %d", n)
	}
	if _, err := os.Stat(filepath.Join(dir, "x.cbz.part")); !os.IsNotExist(err) {
		t.Error("el .part sigue ahí")
	}
	e.SetPaused(true)
	e.dispatch(context.Background())
	if len(h.order) != 0 {
		t.Error("en pausa no debería empezar nada")
	}
	e.SetPaused(false)
	e.St.DB.Exec(`UPDATE series SET paused=1 WHERE id=?`, sid)
	e.dispatch(context.Background())
	if len(h.order) != 0 {
		t.Error("una serie pausada no debería descargar")
	}
}

// Dos capítulos cuyos nombres larguísimos coinciden en los primeros 150 caracteres no se pisan el archivo.
func TestLongNamesDontCollide(t *testing.T) {
	e, h := newTestEngine(t)
	long := strings.Repeat("Un título larguísimo de verdad ", 8)
	h.mu.Lock()
	h.chapters["A"] = map[string][]Chapter{"/m": {
		{URL: "1", Name: long + " parte uno", Number: -1},
		{URL: "2", Name: long + " parte dos", Number: -1},
	}}
	h.mu.Unlock()
	e.Follow(context.Background(), "A", Manga{URL: "/m", Title: long}, "", true)
	drain(t, e)
	if n := count(e, `SELECT count(DISTINCT cbz) FROM chapter WHERE state='downloaded'`); n != 2 {
		t.Fatalf("archivos distintos: %d, esperado 2", n)
	}
}

func TestChapterNumber(t *testing.T) {
	for name, want := range map[string]float64{
		"Capítulo 12":          12,
		"Chapter 3.5: Extra":   3.5,
		"Cap. 7,5":             7.5,
		"Vol. 2 Ch. 15":        15,
		"Episodio 4":           4,
		"#22 El final":         22,
		"Oneshot":              -1,
		"Volumen 3 sin número": 3,
	} {
		if got := chapterNumber(Chapter{Name: name, Number: -1}); got != want {
			t.Errorf("%q → %g, esperado %g", name, got, want)
		}
	}
	if got := chapterNumber(Chapter{Name: "Capítulo 99", Number: 7}); got != 7 {
		t.Errorf("el número de la extensión manda: %g", got)
	}
}

func FuzzChapterNumber(f *testing.F) {
	for _, s := range []string{"Capítulo 1", "ch.2,5", "#", "1e308", "Cap 99999999999999999999"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		n := chapterNumber(Chapter{Name: s, Number: -1})
		if n != -1 && n < 0 {
			t.Fatalf("%q → %g", s, n)
		}
	})
}
