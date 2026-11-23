package api

import (
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"

	"rensaigo/internal/hostproc"
)

// La API JSON dice de quién es el error: un fallo propio (base de datos, extensión que no existe) no es «host».
func TestAPIErrorKind(t *testing.T) {
	env := newTestEnv(t)
	w := env.do(t, "POST", "/api/install", url.Values{"pkg": {"no.existe"}})
	var m map[string]string
	json.Unmarshal(w.Body.Bytes(), &m)
	if w.Code != 502 || m["kind"] != "internal" || m["error"] == "" {
		t.Errorf("instalar algo que no existe: %d %v", w.Code, m)
	}
	w = env.do(t, "GET", "/api/search?source=A&q=x", nil)
	if w.Code != 200 {
		t.Errorf("buscar: %d %s", w.Code, w.Body.String())
	}
}

// Una extensión que necesita WebView da un mensaje claro, no la traza de Java.
func TestFriendlyErr(t *testing.T) {
	cases := map[error]string{
		&hostproc.Error{Kind: "webview", Message: "webview: java.lang.NoClassDefFoundError: org/cef/handler/CefRenderHandlerAdapter"}: "WebView",
		&hostproc.Error{Kind: "http", Message: "HTTP error 503"}:                                                                      "503",
		errors.New("otra cosa"): "otra cosa",
	}
	for err, want := range cases {
		if got := friendlyErr(err); !strings.Contains(got, want) || strings.Contains(got, "NoClassDefFoundError") {
			t.Errorf("%v → %q", err, got)
		}
	}
	if errKind(errors.New("database is locked")) != "internal" || errKind(&hostproc.Error{Kind: "network"}) != "network" {
		t.Error("errKind")
	}
}
