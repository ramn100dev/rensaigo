package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"rensaigo/internal/notify"
)

// Los capítulos nuevos se juntan en un mensaje por tanda; los avisos de la campana salen una vez cada uno.
func TestNotifications(t *testing.T) {
	env := newTestEnv(t) // ya descargó 3 capítulos de «Test Series» (y se apuntaron para avisar)
	s := env.s
	var mu sync.Mutex
	var got []notify.Msg
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m notify.Msg
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &m)
		mu.Lock()
		got = append(got, m)
		mu.Unlock()
	}))
	defer srv.Close()
	take := func() []notify.Msg {
		mu.Lock()
		defer mu.Unlock()
		out := got
		got = nil
		return out
	}
	s.saveChannels([]notify.Channel{{ID: "a", Kind: "webhook", URL: srv.URL}})
	s.St.Set("notify.base", "https://manga.example/")
	ctx := context.Background()

	s.flushChapters(ctx)
	ms := take()
	if len(ms) != 1 || ms[0].Title != "Test Series" || !strings.Contains(ms[0].Text, "3") || ms[0].Link != "https://manga.example/library/1" {
		t.Fatalf("una serie, tres capítulos: %+v", ms)
	}
	s.flushChapters(ctx)
	if ms := take(); len(ms) != 0 {
		t.Fatalf("se repitió el aviso: %+v", ms)
	}
	// dos series en la misma tanda: un solo mensaje con las dos
	s.OnDownloaded(1, "Test Series", 4)
	s.OnDownloaded(2, "Old One", 2)
	s.flushChapters(ctx)
	ms = take()
	if len(ms) != 1 || !strings.Contains(ms[0].Text, "Test Series") || !strings.Contains(ms[0].Text, "Old One") {
		t.Fatalf("dos series: %+v", ms)
	}
	// un canal que solo quiere avisos no recibe capítulos
	s.saveChannels([]notify.Channel{{ID: "a", Kind: "webhook", URL: srv.URL, Events: "alerts"}})
	s.OnDownloaded(1, "Test Series", 5)
	s.flushChapters(ctx)
	if ms := take(); len(ms) != 0 {
		t.Fatalf("canal solo de avisos recibió capítulos: %+v", ms)
	}

	// un capítulo fallido: aviso una vez; si se arregla y vuelve a fallar, otra vez
	s.St.DB.Exec(`UPDATE chapter SET state='failed', error='HTTP error 404' WHERE id=?`, env.ch)
	s.dropAlertCache()
	s.notifyAlerts(ctx)
	if ms := take(); len(ms) != 1 || ms[0].Kind != "alerts" {
		t.Fatalf("aviso de fallidos: %+v", ms)
	}
	s.dropAlertCache()
	s.notifyAlerts(ctx)
	if ms := take(); len(ms) != 0 {
		t.Fatalf("el aviso se repitió: %+v", ms)
	}
	s.St.DB.Exec(`UPDATE chapter SET state='downloaded', error='' WHERE id=?`, env.ch)
	s.dropAlertCache()
	s.notifyAlerts(ctx)
	s.St.DB.Exec(`UPDATE chapter SET state='failed', error='HTTP error 404' WHERE id=?`, env.ch)
	s.dropAlertCache()
	s.notifyAlerts(ctx)
	if ms := take(); len(ms) != 1 {
		t.Fatalf("al volver a fallar: %+v", ms)
	}
}

// El rensaigo.json de cada carpeta se escribe con lo leído y los sources, y no se reescribe si nada cambió.
func TestSidecarFlush(t *testing.T) {
	env := newTestEnv(t)
	s := env.s
	s.St.DB.Exec(`INSERT INTO progress(chapter_id,page,done,updated) VALUES(?,0,1,1) ON CONFLICT(chapter_id) DO UPDATE SET done=1`, env.ch)
	s.Touch(env.series)
	s.flushSidecars(context.Background())
	var path string
	s.St.DB.QueryRow(`SELECT path FROM series WHERE id=?`, env.series).Scan(&path)
	b, err := os.ReadFile(path + "/rensaigo.json")
	if err != nil {
		t.Fatal(err)
	}
	var sc struct {
		Sources []struct {
			SourceID string `json:"source_id"`
		}
		Read []string
	}
	json.Unmarshal(b, &sc)
	if len(sc.Sources) == 0 || sc.Sources[0].SourceID != "A" || len(sc.Read) != 1 {
		t.Fatalf("rensaigo.json: %s", b)
	}
}
