package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"rensaigo/internal/config"
	"rensaigo/internal/extrepo"
	"rensaigo/internal/hostproc"
	"rensaigo/internal/i18n"
	"rensaigo/internal/jobs"
	"rensaigo/internal/library"
	"rensaigo/internal/store"
)

// Host falso para recorrer la web sin JVM: cada source tiene un manga con tres capítulos de tres páginas.
type webHost struct{}

var tinyPNG = func() []byte {
	var b bytes.Buffer
	png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 8, 12)))
	return b.Bytes()
}()

func (webHost) CallJar(ctx context.Context, jar, op string, args map[string]any) (json.RawMessage, error) {
	switch op {
	case "search", "popular":
		return json.Marshal(M{"mangas": []M{{"url": "/m", "title": "Test Series"}, {"url": "/n", "title": "Another One"}}})
	case "details":
		var cs []M
		for i := 1; i <= 3; i++ {
			cs = append(cs, M{"url": fmt.Sprintf("/c%d", i), "name": fmt.Sprintf("Chapter %d", i), "number": float64(i)})
		}
		return json.Marshal(M{"manga": M{"title": "Test Series", "author": "Someone", "genre": "Action, Drama", "status": 1}, "chapters": cs})
	case "pages":
		return json.Marshal(M{"pages": []M{{"index": 0}, {"index": 1}, {"index": 2}}})
	case "image":
		return json.RawMessage(`{}`), os.WriteFile(args["out"].(string), tinyPNG, 0o644)
	case "prefs":
		return json.Marshal(M{"prefs": []M{
			{"key": "domain", "title": "Domain", "summary": "Mirror to use", "valueType": "String", "value": "a.com",
				"entries": []string{"A", "B"}, "entryValues": []string{"a.com", "b.com"}, "visible": true, "enabled": true},
			{"key": "hd", "title": "HD", "valueType": "Boolean", "value": true, "visible": true, "enabled": true},
		}})
	case "setpref":
		return json.RawMessage(`{}`), nil
	}
	return nil, &hostproc.Error{Kind: "extension", Message: "unsupported " + op}
}

type webJars struct{}

func (webJars) JarFor(string) (string, error) { return "fake.jar", nil }

// logBuf recoge el log (lo escriben también las gorutinas de fondo).
type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) { l.mu.Lock(); defer l.mu.Unlock(); return l.b.Write(p) }
func (l *logBuf) String() string              { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

type testEnv struct {
	s      *Server
	h      http.Handler
	log    *logBuf
	series int64 // serie seguida y descargada
	ch     int64 // uno de sus capítulos
	imp    int64 // serie importada del disco, sin source
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	tmp := t.TempDir()
	cfg := &config.Config{ConfigDir: filepath.Join(tmp, "config"), LibraryDir: filepath.Join(tmp, "lib"), DefaultCat: "Manga",
		DLConcurrency: 2, PerSource: 1, PageConcurrency: 2, MaxAttempts: 3, CheckInterval: time.Hour, PrefLang: "en", UILang: "en"}
	for _, d := range []string{"tmp", "cache", "extensions"} {
		os.MkdirAll(cfg.Path(d), 0o755)
	}
	st, err := store.Open(cfg.Path("rensaigo.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for _, x := range []struct{ id, pkg, name, lang string }{
		{"A", "pkg.a", "Alpha Scans", "en"}, {"B", "pkg.b", "Beta", "es"}, {"C1", "pkg.multi", "Multi", "en"}, {"C2", "pkg.multi", "Multi", "fr"},
	} {
		st.DB.Exec(`INSERT OR IGNORE INTO ext(pkg,name,version,version_code,jar_path,jar_sha256,installed_at) VALUES(?,?,'1.4.1',1,'','',0)`, x.pkg, x.name)
		st.DB.Exec(`INSERT INTO source(id,pkg,name,lang,base_url,enabled,status,has_prefs) VALUES(?,?,?,?,'https://example.com',1,'ok',1)`, x.id, x.pkg, x.name, x.lang)
	}
	st.DB.Exec(`INSERT INTO repo_ext(pkg,name,lang,version,version_code,nsfw,jar_url,icon_url) VALUES('pkg.new','Newcomer','en','1.0',1,0,'https://x/a.jar','')`)
	host := hostproc.New(hostproc.Options{})
	repo := extrepo.New(st, host, "", "", cfg.Path("extensions"))
	eng := jobs.New(cfg, st, webHost{}, webJars{})
	th := &library.Thumbs{Dir: cfg.Path("cache", "thumbs"), MaxBytes: 1 << 20, Width: 100}
	s, err := New(cfg, st, host, repo, eng, th)
	if err != nil {
		t.Fatal(err)
	}
	eng.OnChange, eng.OnDownloaded = s.Touch, s.OnDownloaded
	lb := &logBuf{}
	log.SetOutput(lb)
	t.Cleanup(func() { log.SetOutput(os.Stderr); i18n.Set("en") })

	env := &testEnv{s: s, h: s.Handler(), log: lb}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	env.series, err = eng.Follow(ctx, "A", jobs.Manga{URL: "/m", Title: "Test Series"}, "", true)
	if err != nil {
		t.Fatal(err)
	}
	go eng.Run(ctx)
	for i := 0; ; i++ {
		var n int
		st.DB.QueryRow(`SELECT count(*) FROM chapter WHERE state='downloaded'`).Scan(&n)
		if n == 3 {
			break
		}
		if i > 300 {
			t.Fatalf("la descarga no terminó (%d de 3)", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
	st.DB.QueryRow(`SELECT id FROM chapter WHERE series_id=? ORDER BY number LIMIT 1`, env.series).Scan(&env.ch)
	st.DB.Exec(`INSERT INTO progress(chapter_id,page,done,updated) VALUES(?,1,0,?)`, env.ch, time.Now().Unix())
	st.DB.Exec(`INSERT INTO tracking(series_id,svc,rid,total,rd) VALUES(?,'mal','1',10,1)`, env.series)
	st.Set("opds.key", "k3y")
	// un source cuya extensión aún no está instalada (restaurado de un rensaigo.json en una instalación nueva)
	st.DB.Exec(`INSERT INTO series_source(series_id,source_id,url,title,priority,temporary) VALUES(?,'999','/x','Test Series',5,1)`, env.series)

	// una serie que ya estaba en disco: se adopta como importada, sin source
	img := filepath.Join(tmp, "p.png")
	os.WriteFile(img, tinyPNG, 0o644)
	if _, err := library.WriteCBZ(filepath.Join(cfg.LibraryDir, "Manga", "Old One", "[Gamma][en] Old One 01.cbz"), []string{img}, &library.ComicInfo{Number: "1"}); err != nil {
		t.Fatal(err)
	}
	s.QuickScan(true)
	st.DB.QueryRow(`SELECT id FROM series WHERE title='Old One'`).Scan(&env.imp)
	if env.imp == 0 {
		t.Fatal("no se adoptó la serie del disco")
	}
	return env
}

func (e *testEnv) do(t *testing.T, method, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	r := httptest.NewRequest(method, path, body)
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	r.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

// Restos típicos de un fallo al pintar: formato mal pasado, valor ausente o contenido que html/template bloquea.
var badMarks = []string{"%!", "<no value>", "ZgotmplZ"}

// Palabras de la interfaz en español que no deberían salir con la interfaz en inglés (los datos de prueba están
// en inglés).
var spanishUI = []string{"Ajustes", "Biblioteca", "Descargar", "capítulos", "Buscar", "Guardar", "Pausar", "leídos", "hace ", "Activar", "Capítulo"}

func TestWebPages(t *testing.T) {
	env := newTestEnv(t)
	ids := strings.NewReplacer("{S}", fmt.Sprint(env.series), "{C}", fmt.Sprint(env.ch), "{I}", fmt.Sprint(env.imp))
	pages := []string{
		"/library", "/library/{S}", "/library/{I}", "/library/grid", "/read/{C}", "/series", "/series/{S}", "/series/{S}/chapters",
		"/series/search?q=test", "/series/{S}/candidates", "/series/{I}/candidates",
		"/series/{S}/candstat?source=B&url=%2Fm&title=Test+Series", "/series/{S}/sswidth",
		"/queue", "/hdr/pill", "/hdr/queue", "/hdr/events", "/notify",
		"/sources", "/sources/repo?q=new", "/sources/coverage", "/sources/A/prefs", "/sources/C1/prefs",
		"/import", "/import/{I}/candidates?q=old", "/system", "/system/opds", "/connections", "/track/list",
		"/opds/k3y", "/opds/k3y/reading", "/opds/k3y/latest", "/opds/k3y/all", "/opds/k3y/search?q=test", "/opds/k3y/series/{S}",
		"/api/status", "/api/sources", "/api/series", "/api/chapters?series={S}", "/api/events", "/api/repo",
	}
	for _, lang := range []string{"en", "es"} {
		i18n.Set(lang)
		for _, p := range pages {
			p = ids.Replace(p)
			w := env.do(t, "GET", p, nil)
			if w.Code == http.StatusSeeOther && (p == "/series" || p == "/import" || strings.HasPrefix(p, "/series/")) {
				continue // rutas antiguas: llevan a la biblioteca o a Ajustes
			}
			if w.Code != 200 {
				t.Errorf("[%s] GET %s: %d %s", lang, p, w.Code, trunc(w.Body.String(), 200))
				continue
			}
			body := w.Body.String()
			for _, m := range badMarks {
				if i := strings.Index(body, m); i >= 0 {
					t.Errorf("[%s] GET %s contiene %q: …%s…", lang, p, m, body[max(0, i-80):min(len(body), i+80)])
				}
			}
			if lang == "en" && !strings.HasPrefix(p, "/api/") {
				for _, word := range spanishUI {
					if i := strings.Index(body, word); i >= 0 {
						t.Errorf("[en] GET %s tiene texto en español %q: …%s…", p, word, body[max(0, i-80):min(len(body), i+80)])
					}
				}
			}
		}
	}
	for _, p := range []string{"/page/{C}/0", "/thumb/{S}", "/thumb/{I}", "/opds/k3y/page/{C}/1", "/opds/k3y/cbz/{C}", "/opds/k3y/thumb/{S}"} {
		p = ids.Replace(p)
		if w := env.do(t, "GET", p, nil); w.Code != 200 || w.Body.Len() == 0 {
			t.Errorf("GET %s: %d (%d bytes)", p, w.Code, w.Body.Len())
		}
	}
	for _, p := range []string{"/library/999", "/read/999", "/page/{C}/99", "/opds/wrong", "/opds/k3y/cbz/999"} {
		if w := env.do(t, "GET", ids.Replace(p), nil); w.Code != 404 {
			t.Errorf("GET %s: %d, esperado 404", p, w.Code)
		}
	}
	if l := env.log.String(); strings.Contains(l, "plantilla") || strings.Contains(l, "panic") {
		t.Errorf("errores en el log:\n%s", l)
	}
}

// Las acciones de la ficha y de Sources responden sin error y hacen lo que dicen.
func TestWebActions(t *testing.T) {
	env := newTestEnv(t)
	sp := func(path string, form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		w := env.do(t, "POST", path, form)
		if w.Code >= 400 {
			t.Errorf("POST %s: %d %s", path, w.Code, trunc(w.Body.String(), 200))
		}
		return w
	}
	one := func(q string, args ...any) (v int64) {
		env.s.St.DB.QueryRow(q, args...).Scan(&v)
		return
	}
	S := fmt.Sprintf("/series/%d/", env.series)
	for _, a := range []string{"pause", "resume", "fav", "auto", "auto", "verify", "check", "retry-failed", "queue"} {
		sp(S+a, url.Values{})
	}
	if one(`SELECT favorite FROM series WHERE id=?`, env.series) != 1 {
		t.Error("fav no marcó la serie")
	}
	sp(fmt.Sprintf("/chapter/%d/read", env.ch), url.Values{"done": {"1"}})
	if one(`SELECT done FROM progress WHERE chapter_id=?`, env.ch) != 1 {
		t.Error("marcar como leído no se guardó")
	}
	sp(fmt.Sprintf("/chapter/%d/read", env.ch), url.Values{"done": {"0"}})
	if one(`SELECT count(*) FROM progress WHERE chapter_id=?`, env.ch) != 0 {
		t.Error("desmarcar no borró el progreso")
	}
	sp("/api/progress", url.Values{"chapter": {fmt.Sprint(env.ch)}, "page": {"2"}, "done": {"1"}})
	if one(`SELECT page FROM progress WHERE chapter_id=?`, env.ch) != 2 {
		t.Error("el progreso del lector no se guardó")
	}
	// añadir otro source a la serie, ordenarlo y quitarlo
	sp("/series/follow", url.Values{"source": {"B"}, "url": {"/m"}, "title": {"Test Series"}, "series": {fmt.Sprint(env.series)}})
	var ssA, ssB int64
	env.s.St.DB.QueryRow(`SELECT id FROM series_source WHERE series_id=? AND source_id='A'`, env.series).Scan(&ssA)
	env.s.St.DB.QueryRow(`SELECT id FROM series_source WHERE series_id=? AND source_id='B'`, env.series).Scan(&ssB)
	if ssB == 0 {
		t.Fatal("no se añadió el segundo source")
	}
	sp(S+"order", url.Values{"ss": {fmt.Sprint(ssB), fmt.Sprint(ssA)}})
	if one(`SELECT temporary FROM series_source WHERE id=?`, ssA) != 1 {
		t.Error("tras reordenar, A debería ser temporal")
	}
	sp(S+"from", url.Values{"ss": {fmt.Sprint(ssB)}, "from": {"2"}})
	sp(S+"unsource", url.Values{"ss": {fmt.Sprint(ssB)}})
	if one(`SELECT count(*) FROM series_source WHERE id=?`, ssB) != 0 {
		t.Error("no se quitó el source")
	}
	// importada: emparejarla con un source
	sp(fmt.Sprintf("/import/%d/match", env.imp), url.Values{"source": {"A"}, "url": {"/n"}, "title": {"Old One"}})
	if one(`SELECT count(*) FROM series_source WHERE series_id=?`, env.imp) != 1 {
		t.Error("la importada no quedó emparejada")
	}

	// Sources
	sp("/sources/A/toggle", nil)
	sp("/sources/A/toggle", nil)
	sp("/sources/A/prefs", url.Values{"key": {"hd"}, "type": {"Boolean"}, "value": {"false"}})
	sp("/sources/C1/lang", url.Values{"sib": {"C2"}})
	if one(`SELECT enabled FROM source WHERE id='C2'`) != 0 {
		t.Error("el idioma fr debería haberse desactivado")
	}
	if w := env.do(t, "POST", "/sources/C1/lang", url.Values{"sib": {"A"}}); w.Code != 404 {
		t.Errorf("activar un source de otra extensión desde el modal: %d", w.Code)
	}
	sp("/downloads/pause", nil)
	sp("/downloads/pause", nil)

	// ajustes: cambiar el idioma recarga la página y se aplica
	w := sp("/settings", url.Values{"ui_lang": {"es"}})
	if i18n.Lang() != "es" || w.Header().Get("HX-Refresh") == "" {
		t.Errorf("cambiar el idioma: lang=%s, refresh=%q", i18n.Lang(), w.Header().Get("HX-Refresh"))
	}
	before := env.s.Cfg.DLConcurrency
	if w := env.do(t, "POST", "/settings", url.Values{"dl_concurrency": {"-4"}}); env.s.Cfg.DLConcurrency != before || !strings.Contains(w.Body.String(), `class="bad`) {
		t.Errorf("un ajuste no válido se aceptó (ahora %d)", env.s.Cfg.DLConcurrency)
	}

	// borrar la serie se lleva su carpeta
	dir := filepath.Join(env.s.Cfg.LibraryDir, "Manga", "Test Series")
	sp(S+"delete", nil)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("la carpeta de la serie borrada sigue ahí")
	}
	if l := env.log.String(); strings.Contains(l, "plantilla") || strings.Contains(l, "panic") {
		t.Errorf("errores en el log:\n%s", l)
	}
}

// Una petición que cambia algo y viene de otra web se rechaza; con contraseña, sin sesión no se entra.
func TestWebSecurity(t *testing.T) {
	env := newTestEnv(t)
	r := httptest.NewRequest("POST", fmt.Sprintf("/series/%d/delete", env.series), nil)
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	w := httptest.NewRecorder()
	env.h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("POST desde otra web: %d, esperado 403", w.Code)
	}
	env.s.Cfg.Password = "secreto"
	h := env.s.Handler()
	for _, p := range []string{"/library", "/api/series", "/page/1/0"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", p, nil))
		if w.Code != http.StatusSeeOther && w.Code != http.StatusUnauthorized {
			t.Errorf("sin sesión GET %s: %d", p, w.Code)
		}
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/opds/k3y", nil))
	if w.Code != 200 {
		t.Errorf("OPDS con su clave no necesita sesión: %d", w.Code)
	}
	form := strings.NewReader("password=secreto")
	r = httptest.NewRequest("POST", "/login", form)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	c := w.Result().Cookies()
	if len(c) == 0 {
		t.Fatal("el login no dio cookie")
	}
	r = httptest.NewRequest("GET", "/library", nil)
	r.AddCookie(c[0])
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Errorf("con sesión GET /library: %d", w.Code)
	}
}
