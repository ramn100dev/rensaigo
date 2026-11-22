package notify

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSend(t *testing.T) {
	var got struct{ path, body, title, click, auth string }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.path, got.body, got.title, got.click, got.auth = r.URL.RequestURI(), string(b), r.Header.Get("Title"), r.Header.Get("Click"), r.Header.Get("Authorization")
	}))
	defer srv.Close()
	m := Msg{Title: "Galaxias", Text: "Nuevo: capítulo 36", Link: "http://x/library/1", Kind: "chapters", Level: "info"}
	for _, c := range []struct {
		ch   Channel
		want []string
	}{
		{Channel{Kind: "ntfy", URL: srv.URL + "/tema", Token: "tk"}, []string{"/tema", "Nuevo: capítulo 36"}},
		{Channel{Kind: "discord", URL: srv.URL + "/hook"}, []string{"/hook", `**Galaxias**`, `\u003chttp://x/library/1`}},
		{Channel{Kind: "gotify", URL: srv.URL, Token: "abc"}, []string{"/message?token=abc", `"priority":4`}},
		{Channel{Kind: "webhook", URL: srv.URL + "/w"}, []string{"/w", `"kind":"chapters"`, `"url":"http://x/library/1"`}},
	} {
		if err := c.ch.Validate(); err != nil {
			t.Fatalf("%s: %v", c.ch.Kind, err)
		}
		if err := Send(context.Background(), c.ch, m); err != nil {
			t.Fatalf("%s: %v", c.ch.Kind, err)
		}
		all := got.path + " " + got.body
		for _, w := range c.want {
			if !strings.Contains(all, w) {
				t.Errorf("%s: falta %q en %q", c.ch.Kind, w, all)
			}
		}
		if c.ch.Kind == "ntfy" && (got.title != "Galaxias" || got.click != m.Link || got.auth != "Bearer tk") {
			t.Errorf("ntfy: cabeceras %+v", got)
		}
	}
	if (Channel{Kind: "ntfy", URL: "https://ntfy.sh"}).Validate() == nil {
		t.Error("ntfy sin tema debería fallar")
	}
	if !(Channel{Events: "alerts"}).Wants("alerts") || (Channel{Events: "none"}).Wants("chapters") {
		t.Error("Wants")
	}
}
