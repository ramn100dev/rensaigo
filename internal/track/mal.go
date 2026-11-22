package track

// MyAnimeList, API v2: OAuth2 con PKCE «plain». La app la registra el propio usuario en
// https://myanimelist.net/apiconfig y pega aquí su Client ID; los tokens van en los ajustes mal.*.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"rensaigo/internal/i18n"
	"strconv"
	"strings"
	"sync"
	"time"

	"rensaigo/internal/store"
)

const (
	malAPI       = "https://api.myanimelist.net/v2"
	malAuthURL   = "https://myanimelist.net/v1/oauth2/authorize"
	malTokenURL  = "https://myanimelist.net/v1/oauth2/token"
	malMangaInfo = "id,title,main_picture,alternative_titles,synopsis,genres,status,authors{first_name,last_name},num_chapters,my_list_status"
)

type MAL struct {
	kv
	HTTP *http.Client
	mu   sync.Mutex // serializa el refresco del token
	g    gate
}

func NewMAL(st *store.Store) *MAL {
	return &MAL{kv: kv{st, "mal"}, HTTP: &http.Client{Timeout: 30 * time.Second}, g: gate{every: 500 * time.Millisecond}}
}

func (c *MAL) Key() string      { return "mal" }
func (c *MAL) Name() string     { return "MyAnimeList" }
func (c *MAL) ClientID() string { return c.get("client_id") }
func (c *MAL) User() string     { return c.get("user") }
func (c *MAL) Connected() bool  { return c.get("refresh") != "" }
func (c *MAL) Statuses() []Status {
	return []Status{{"plan_to_read", "Pendientes"}, {"reading", "Leyendo"}, {"on_hold", "En pausa"}}
}

func (c *MAL) SetCredentials(id, secret string) { c.setCredentials(id, secret) }

// ---------------------------------------------------------------- tipos de la API

type malManga struct {
	ID          int64 `json:"id"`
	Title       string
	MainPicture struct {
		Medium string `json:"medium"`
		Large  string `json:"large"`
	} `json:"main_picture"`
	Alt struct {
		Synonyms []string `json:"synonyms"`
		En       string   `json:"en"`
		Ja       string   `json:"ja"`
	} `json:"alternative_titles"`
	Synopsis string `json:"synopsis"`
	Genres   []struct {
		Name string `json:"name"`
	} `json:"genres"`
	Status  string `json:"status"` // finished | currently_publishing | on_hiatus | discontinued | not_yet_published
	Authors []struct {
		Node struct {
			First string `json:"first_name"`
			Last  string `json:"last_name"`
		} `json:"node"`
	} `json:"authors"`
	NumChapters int            `json:"num_chapters"`
	MyStatus    *malListStatus `json:"my_list_status"`
}

type malListStatus struct {
	Status          string `json:"status"`
	NumChaptersRead int    `json:"num_chapters_read"`
}

func (m *malManga) entry() Entry {
	e := Entry{ID: strconv.FormatInt(m.ID, 10), Title: m.Title, En: m.Alt.En, Alt: append(append([]string{}, m.Alt.Synonyms...), m.Alt.Ja),
		Pic: m.MainPicture.Large, PicSmall: m.MainPicture.Medium, Synopsis: m.Synopsis, Total: m.NumChapters,
		URL: "https://myanimelist.net/manga/" + strconv.FormatInt(m.ID, 10)}
	if e.Pic == "" {
		e.Pic = e.PicSmall
	}
	var g, a []string
	for _, x := range m.Genres {
		g = append(g, x.Name)
	}
	for _, x := range m.Authors { // MAL lo da como «Apellido, Nombre»: aquí «Nombre Apellido»
		if n := strings.TrimSpace(x.Node.First + " " + x.Node.Last); n != "" {
			a = append(a, n)
		}
	}
	e.Genres, e.Authors = strings.Join(g, ", "), strings.Join(a, ", ")
	switch m.Status {
	case "finished":
		e.Pub = 2
	case "on_hiatus":
		e.Pub = 6
	case "discontinued":
		e.Pub = 5
	case "currently_publishing", "not_yet_published":
		e.Pub = 1
	}
	if m.MyStatus != nil {
		e.MyRead, e.MyStatus = m.MyStatus.NumChaptersRead, m.MyStatus.Status
	}
	return e
}

// ---------------------------------------------------------------- conexión (OAuth2 + PKCE)

func randString(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)[:n]
}

// AuthURL prepara el inicio de sesión. MAL solo admite PKCE «plain»: el verificador viaja tal cual como challenge.
func (c *MAL) AuthURL(redirect string) (string, error) {
	id := c.ClientID()
	if id == "" {
		return "", i18n.Errorf("falta el Client ID de MyAnimeList")
	}
	verifier, state := randString(96), randString(24)
	c.set("verifier", verifier)
	c.set("state", state)
	q := url.Values{"response_type": {"code"}, "client_id": {id}, "code_challenge": {verifier},
		"code_challenge_method": {"plain"}, "state": {state}, "redirect_uri": {redirect}}
	return malAuthURL + "?" + q.Encode(), nil
}

// Exchange cambia el código de la vuelta de MAL por los tokens y guarda el nombre de usuario.
func (c *MAL) Exchange(ctx context.Context, code, state, redirect string) error {
	if state == "" || state != c.get("state") {
		return i18n.Errorf("la respuesta de MyAnimeList no corresponde a este inicio de sesión (state)")
	}
	f := url.Values{"client_id": {c.ClientID()}, "grant_type": {"authorization_code"}, "code": {code},
		"code_verifier": {c.get("verifier")}, "redirect_uri": {redirect}}
	if sec := c.get("client_secret"); sec != "" {
		f.Set("client_secret", sec)
	}
	if err := c.tokenRequest(ctx, f); err != nil {
		return err
	}
	c.set("state", "")
	var me struct {
		Name string `json:"name"`
	}
	if err := c.do(ctx, "GET", "/users/@me", nil, nil, &me); err != nil {
		return err
	}
	c.set("user", me.Name)
	return nil
}

func (c *MAL) Disconnect() { c.clear("access", "refresh", "expires", "user", "verifier", "state") }

func (c *MAL) tokenRequest(ctx context.Context, f url.Values) error {
	req, _ := http.NewRequestWithContext(ctx, "POST", malTokenURL, strings.NewReader(f.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		if f.Get("grant_type") == "refresh_token" && (resp.StatusCode == 400 || resp.StatusCode == 401) {
			return ErrSessionExpired
		}
		return i18n.Errorf("MyAnimeList rechazó el token (%d): %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var t struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		Expires int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(b, &t); err != nil || t.Access == "" {
		return i18n.Errorf("respuesta de token de MyAnimeList inválida")
	}
	c.set("access", t.Access)
	c.set("refresh", t.Refresh)
	c.set("expires", strconv.FormatInt(time.Now().Unix()+t.Expires, 10))
	return nil
}

// token devuelve un access token válido, refrescándolo si caduca en menos de un día.
func (c *MAL) token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.Connected() {
		return "", ErrNotConnected
	}
	exp, _ := strconv.ParseInt(c.get("expires"), 10, 64)
	if time.Now().Unix() < exp-86400 {
		return c.get("access"), nil
	}
	f := url.Values{"client_id": {c.ClientID()}, "grant_type": {"refresh_token"}, "refresh_token": {c.get("refresh")}}
	if sec := c.get("client_secret"); sec != "" {
		f.Set("client_secret", sec)
	}
	if err := c.tokenRequest(ctx, f); err != nil {
		if errors.Is(err, ErrSessionExpired) {
			c.Disconnect()
		}
		return "", err
	}
	return c.get("access"), nil
}

// ---------------------------------------------------------------- API

func (c *MAL) do(ctx context.Context, method, path string, q url.Values, body url.Values, out any) error {
	tok, err := c.token(ctx)
	if err != nil {
		return err
	}
	if err := c.g.wait(ctx); err != nil {
		return err
	}
	u := malAPI + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rd io.Reader
	if body != nil {
		rd = strings.NewReader(body.Encode())
	}
	req, _ := http.NewRequestWithContext(ctx, method, u, rd)
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode == 401 {
		return i18n.Errorf("MyAnimeList ha rechazado la sesión: vuelve a conectar desde Ajustes")
	}
	if resp.StatusCode/100 != 2 {
		return i18n.Errorf("MyAnimeList respondió %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

// Search busca mangas por título (MAL exige al menos 3 caracteres y como mucho 64).
func (c *MAL) Search(ctx context.Context, q string) ([]Entry, error) {
	q = strings.TrimSpace(q)
	if r := []rune(q); len(r) > 64 {
		q = string(r[:64])
	}
	if len([]rune(q)) < 3 {
		return nil, nil
	}
	var r struct {
		Data []struct {
			Node malManga `json:"node"`
		} `json:"data"`
	}
	if err := c.do(ctx, "GET", "/manga", url.Values{"q": {q}, "limit": {"10"}, "fields": {malMangaInfo}}, nil, &r); err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(r.Data))
	for _, d := range r.Data {
		out = append(out, d.Node.entry())
	}
	return out, nil
}

func (c *MAL) Get(ctx context.Context, id string) (*Entry, error) {
	var m malManga
	if err := c.do(ctx, "GET", "/manga/"+url.PathEscape(id), url.Values{"fields": {malMangaInfo}}, nil, &m); err != nil {
		return nil, err
	}
	e := m.entry()
	return &e, nil
}

// List devuelve la lista del usuario con un estado (plan_to_read, reading, on_hold…). Pagina de 1000 en 1000.
func (c *MAL) List(ctx context.Context, status string) ([]Entry, error) {
	var out []Entry
	q := url.Values{"status": {status}, "limit": {"1000"}, "fields": {"list_status," + malMangaInfo}, "nsfw": {"true"}}
	for page := 0; page < 10; page++ {
		q.Set("offset", strconv.Itoa(page*1000))
		var r struct {
			Data []struct {
				Node       malManga      `json:"node"`
				ListStatus malListStatus `json:"list_status"`
			} `json:"data"`
			Paging struct {
				Next string `json:"next"`
			} `json:"paging"`
		}
		if err := c.do(ctx, "GET", "/users/@me/mangalist", q, nil, &r); err != nil {
			return nil, err
		}
		for _, d := range r.Data {
			m := d.Node
			ls := d.ListStatus
			m.MyStatus = &ls
			out = append(out, m.entry())
		}
		if r.Paging.Next == "" {
			break
		}
	}
	return out, nil
}

func (c *MAL) Push(ctx context.Context, id string, read, total int) error {
	f := url.Values{"num_chapters_read": {strconv.Itoa(read)},
		"status": {statusFor(read, total, "plan_to_read", "reading", "completed")}}
	return c.do(ctx, "PATCH", "/manga/"+url.PathEscape(id)+"/my_list_status", nil, f, nil)
}
