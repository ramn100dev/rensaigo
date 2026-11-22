// Package track: servicios de seguimiento de lectura (MyAnimeList, AniList, MangaDex) detrás de una interfaz común.
// Cada servicio sabe conectarse con la cuenta del usuario, buscar fichas, leer su lista y subir el progreso
// (capítulos leídos). Los tokens se guardan en la tabla de ajustes con el prefijo del servicio («mal.», «anilist.»…).
package track

import (
	"context"
	"html"
	"regexp"
	"rensaigo/internal/i18n"
	"strings"
	"sync"
	"time"

	"rensaigo/internal/store"
)

// Entry: una ficha de un servicio, ya normalizada.
type Entry struct {
	ID       string
	Title    string
	En       string   // título en inglés, si lo hay
	Alt      []string // otros títulos (sinónimos, japonés…)
	Pic      string   // portada grande
	PicSmall string   // portada para listas
	Synopsis string
	Genres   string // separados por comas
	Authors  string // separados por comas
	Pub      int    // estado de publicación con los códigos de las extensiones (SManga.status); 0 = desconocido
	Total    int    // capítulos totales; 0 = en curso o desconocido
	MyRead   int    // capítulos leídos en la lista del usuario
	MyStatus string // estado en la lista del usuario ("" = no está en la lista)
	URL      string // ficha en la web
}

// Titles: título principal, inglés y alternativos, sin repetir (para buscar en los sources).
func (e *Entry) Titles() []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range append([]string{e.Title, e.En}, e.Alt...) {
		t = strings.TrimSpace(t)
		if k := strings.ToLower(t); t != "" && !seen[k] {
			seen[k] = true
			out = append(out, t)
		}
	}
	return out
}

// Status: un estado de la lista del usuario que se puede elegir al importar.
type Status struct{ V, L string }

// Service es lo que la app necesita de cada servicio.
type Service interface {
	Key() string  // "mal", "anilist", "mangadex": prefijo de ajustes, rutas y tabla tracking
	Name() string // nombre visible
	Connected() bool
	User() string
	Disconnect()
	Search(ctx context.Context, q string) ([]Entry, error)
	Get(ctx context.Context, id string) (*Entry, error) // incluye MyRead
	List(ctx context.Context, status string) ([]Entry, error)
	Statuses() []Status // estados de lista que se ofrecen para importar (el primero es el de por defecto)
	// Push deja en la lista del usuario read capítulos leídos (y el estado que toque: pendiente, leyendo, completado).
	Push(ctx context.Context, id string, read, total int) error
}

// OAuth: servicios que se conectan saltando a su web para iniciar sesión (MyAnimeList, AniList).
type OAuth interface {
	ClientID() string
	HasSecret() bool
	SetCredentials(id, secret string) // un secreto vacío con el mismo ID conserva el guardado
	AuthURL(redirect string) (string, error)
	Exchange(ctx context.Context, code, state, redirect string) error
}

var ErrNotConnected = i18n.Errorf("no conectado")

// ErrSessionExpired: el servicio ya no acepta la sesión guardada (refresh token caducado o revocado). El cliente se
// desconecta solo, para que la UI lo diga en vez de fallar en cada sincronización.
var ErrSessionExpired = i18n.Errorf("la sesión ha caducado: vuelve a conectar desde Ajustes → Conexiones")

// gate separa las peticiones a un servicio al menos every (límites de cada API). Lo comparten todas las llamadas del
// cliente, así que vale igual para un bucle que para peticiones sueltas que coincidan.
type gate struct {
	mu    sync.Mutex
	next  time.Time
	every time.Duration
}

func (g *gate) wait(ctx context.Context) error {
	g.mu.Lock()
	now := time.Now()
	at := g.next
	if at.Before(now) {
		at = now
	}
	g.next = at.Add(g.every)
	g.mu.Unlock()
	if d := time.Until(at); d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// setCredentials guarda el ID y el secreto; si el secreto viene vacío y el ID no cambia, se conserva el anterior
// (así reconectar no obliga a pegarlo otra vez).
func (k kv) setCredentials(id, secret string) {
	id, secret = strings.TrimSpace(id), strings.TrimSpace(secret)
	if secret == "" && id == k.get("client_id") {
		secret = k.get("client_secret")
	}
	k.set("client_id", id)
	k.set("client_secret", secret)
}

func (k kv) HasSecret() bool { return k.get("client_secret") != "" }

// kv: ajustes de un servicio con su prefijo.
type kv struct {
	st     *store.Store
	prefix string
}

func (k kv) get(key string) string { return k.st.Get(k.prefix+"."+key, "") }
func (k kv) set(key, v string)     { k.st.Set(k.prefix+"."+key, v) }
func (k kv) clear(keys ...string) {
	for _, x := range keys {
		k.set(x, "")
	}
}

var tagRe = regexp.MustCompile(`<[^>]*>`)

// plain quita las etiquetas HTML y las entidades de una sinopsis.
func plain(s string) string {
	s = strings.ReplaceAll(s, "<br>", "\n")
	s = tagRe.ReplaceAllString(s, "")
	return strings.TrimSpace(html.UnescapeString(s))
}

// statusFor: el estado que corresponde en la lista según lo leído.
func statusFor(read, total int, planned, reading, completed string) string {
	switch {
	case read <= 0:
		return planned
	case total > 0 && read >= total:
		return completed
	}
	return reading
}
