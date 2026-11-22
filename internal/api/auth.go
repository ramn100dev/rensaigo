package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"rensaigo/internal/i18n"
)

// Contraseña opcional (RENSAIGO_PASSWORD): una sola clave para todo, con una cookie que sobrevive a los
// reinicios. Sin contraseña no hay inicio de sesión.

func (s *Server) auth(next http.Handler) http.Handler {
	if s.Cfg.Password == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /opds/ lleva su propia clave en la ruta (las apps de lectura no manejan cookies)
		if r.URL.Path == "/login" || strings.HasPrefix(r.URL.Path, "/static/") || strings.HasPrefix(r.URL.Path, "/opds/") {
			next.ServeHTTP(w, r)
			return
		}
		if c, err := r.Cookie("rensaigo"); err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.session())) == 1 {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") || r.Header.Get("HX-Request") != "" {
			w.Header().Set("HX-Redirect", "/login")
			http.Error(w, "no autenticado", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
}

// session: el valor de la cookie. Depende de la contraseña: al cambiarla, las sesiones abiertas dejan de valer.
func (s *Server) session() string {
	h := sha256.Sum256([]byte(s.token + "\x00" + s.Cfg.Password))
	return hex.EncodeToString(h[:])
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		time.Sleep(300 * time.Millisecond) // freno simple a la fuerza bruta
		if subtle.ConstantTimeCompare([]byte(r.FormValue("password")), []byte(s.Cfg.Password)) == 1 {
			http.SetCookie(w, &http.Cookie{Name: "rensaigo", Value: s.session(), Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 30 * 86400})
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		s.render(w, "login", M{"Error": i18n.T("Contraseña incorrecta")})
		return
	}
	s.render(w, "login", M{})
}
