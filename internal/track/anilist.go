package track

// AniList, API GraphQL. Conexión OAuth2 con código: el usuario crea un cliente en
// https://anilist.co/settings/developer (Redirect URL = la que enseña la app) y pega aquí el ID y el secreto.
// El token dura un año y no se puede refrescar: al caducar hay que volver a conectar.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"rensaigo/internal/i18n"
	"strconv"
	"strings"
	"time"

	"rensaigo/internal/store"
)

const (
	alAPI      = "https://graphql.anilist.co"
	alAuthURL  = "https://anilist.co/api/v2/oauth/authorize"
	alTokenURL = "https://anilist.co/api/v2/oauth/token"
	alMedia    = `fragment m on Media { id siteUrl title { romaji english native } synonyms coverImage { large medium }
		chapters status description(asHtml: false) genres
		staff(perPage: 6, sort: [RELEVANCE]) { edges { role node { name { full } } } }
		mediaListEntry { progress status } }`
)

type AniList struct {
	kv
	HTTP *http.Client
	g    gate
}

func NewAniList(st *store.Store) *AniList {
	// AniList admite 90 peticiones por minuto, pero lleva tiempo «degradado» a 30: se va a una cada 2,1 s
	return &AniList{kv: kv{st, "anilist"}, HTTP: &http.Client{Timeout: 30 * time.Second}, g: gate{every: 2100 * time.Millisecond}}
}

func (c *AniList) Key() string      { return "anilist" }
func (c *AniList) Name() string     { return "AniList" }
func (c *AniList) ClientID() string { return c.get("client_id") }
func (c *AniList) User() string     { return c.get("user") }
func (c *AniList) Statuses() []Status {
	return []Status{{"PLANNING", "Pendientes"}, {"CURRENT", "Leyendo"}, {"PAUSED", "En pausa"}}
}

func (c *AniList) Connected() bool { return c.get("access") != "" && !c.Expired() }

// Expired: había sesión pero ya pasó su año (AniList no deja renovarla: hay que volver a conectar).
func (c *AniList) Expired() bool {
	exp, _ := strconv.ParseInt(c.get("expires"), 10, 64)
	return c.get("access") != "" && exp > 0 && time.Now().Unix() >= exp
}

func (c *AniList) SetCredentials(id, secret string) { c.setCredentials(id, secret) }

func (c *AniList) Disconnect() { c.clear("access", "expires", "user", "user_id", "state") }

func (c *AniList) AuthURL(redirect string) (string, error) {
	if c.ClientID() == "" || c.get("client_secret") == "" {
		return "", i18n.Errorf("faltan el ID o el secreto del cliente de AniList")
	}
	state := randString(24)
	c.set("state", state)
	q := url.Values{"client_id": {c.ClientID()}, "redirect_uri": {redirect}, "response_type": {"code"}, "state": {state}}
	return alAuthURL + "?" + q.Encode(), nil
}

func (c *AniList) Exchange(ctx context.Context, code, state, redirect string) error {
	// AniList devuelve el state si se le manda; si viniera vacío no se puede comprobar, pero el código solo sirve
	// junto con el secreto del cliente, que nunca sale de aquí
	if state != "" && state != c.get("state") {
		return i18n.Errorf("la respuesta de AniList no corresponde a este inicio de sesión (state)")
	}
	body, _ := json.Marshal(map[string]string{"grant_type": "authorization_code", "client_id": c.ClientID(),
		"client_secret": c.get("client_secret"), "redirect_uri": redirect, "code": code})
	req, _ := http.NewRequestWithContext(ctx, "POST", alTokenURL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return i18n.Errorf("AniList rechazó el token (%d): %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var t struct {
		Access  string `json:"access_token"`
		Expires int64  `json:"expires_in"`
	}
	if json.Unmarshal(b, &t) != nil || t.Access == "" {
		return i18n.Errorf("respuesta de token de AniList inválida")
	}
	c.set("access", t.Access)
	if t.Expires > 0 {
		c.set("expires", strconv.FormatInt(time.Now().Unix()+t.Expires, 10))
	}
	c.set("state", "")
	var r struct {
		Viewer struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"Viewer"`
	}
	if err := c.gql(ctx, `query { Viewer { id name } }`, nil, &r); err != nil {
		c.Disconnect()
		return err
	}
	c.set("user", r.Viewer.Name)
	c.set("user_id", strconv.FormatInt(r.Viewer.ID, 10))
	return nil
}

func (c *AniList) gql(ctx context.Context, query string, vars map[string]any, out any) error {
	tok := c.get("access")
	if tok == "" {
		return ErrNotConnected
	}
	if err := c.g.wait(ctx); err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{"query": query, "variables": vars})
	req, _ := http.NewRequestWithContext(ctx, "POST", alAPI, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	var r struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
			Status  int    `json:"status"`
		} `json:"errors"`
	}
	json.Unmarshal(b, &r)
	if resp.StatusCode == 401 || (len(r.Errors) > 0 && r.Errors[0].Status == 401) {
		return i18n.Errorf("AniList ha rechazado la sesión: vuelve a conectar desde Ajustes")
	}
	if resp.StatusCode == 429 {
		return i18n.Errorf("AniList pide ir más despacio: prueba en un minuto")
	}
	if len(r.Errors) > 0 {
		return fmt.Errorf("AniList: %s", r.Errors[0].Message)
	}
	if resp.StatusCode/100 != 2 {
		return i18n.Errorf("AniList respondió %d", resp.StatusCode)
	}
	if out != nil {
		return json.Unmarshal(r.Data, out)
	}
	return nil
}

type alMediaT struct {
	ID      int64  `json:"id"`
	SiteURL string `json:"siteUrl"`
	Title   struct {
		Romaji  string `json:"romaji"`
		English string `json:"english"`
		Native  string `json:"native"`
	} `json:"title"`
	Synonyms   []string `json:"synonyms"`
	CoverImage struct {
		Large  string `json:"large"`
		Medium string `json:"medium"`
	} `json:"coverImage"`
	Chapters    int      `json:"chapters"`
	Status      string   `json:"status"`
	Description string   `json:"description"`
	Genres      []string `json:"genres"`
	Staff       struct {
		Edges []struct {
			Role string `json:"role"`
			Node struct {
				Name struct {
					Full string `json:"full"`
				} `json:"name"`
			} `json:"node"`
		} `json:"edges"`
	} `json:"staff"`
	List *struct {
		Progress int    `json:"progress"`
		Status   string `json:"status"`
	} `json:"mediaListEntry"`
}

// autores: el staff con papel de guion o dibujo (no traductores, rotulistas ni ayudantes).
var alAuthorRole = regexp.MustCompile(`(?i)\b(story|art)\b`)
var alNotAuthor = regexp.MustCompile(`(?i)translat|letter|assist|touch-up|edit`)

func (m *alMediaT) entry() Entry {
	title := m.Title.Romaji
	if title == "" {
		title = m.Title.English
	}
	e := Entry{ID: strconv.FormatInt(m.ID, 10), Title: title, En: m.Title.English,
		Alt: append(append([]string{}, m.Synonyms...), m.Title.Native), Pic: m.CoverImage.Large, PicSmall: m.CoverImage.Medium,
		Synopsis: plain(m.Description), Genres: strings.Join(m.Genres, ", "), Total: m.Chapters, URL: m.SiteURL}
	if e.PicSmall == "" {
		e.PicSmall = e.Pic
	}
	var a []string
	seen := map[string]bool{}
	for _, s := range m.Staff.Edges {
		n := s.Node.Name.Full
		if alAuthorRole.MatchString(s.Role) && !alNotAuthor.MatchString(s.Role) && n != "" && !seen[n] {
			seen[n] = true
			a = append(a, n)
		}
	}
	e.Authors = strings.Join(a, ", ")
	switch m.Status {
	case "FINISHED":
		e.Pub = 2
	case "HIATUS":
		e.Pub = 6
	case "CANCELLED":
		e.Pub = 5
	case "RELEASING", "NOT_YET_RELEASED":
		e.Pub = 1
	}
	if m.List != nil {
		e.MyRead, e.MyStatus = m.List.Progress, m.List.Status
	}
	return e
}

func (c *AniList) Search(ctx context.Context, q string) ([]Entry, error) {
	if strings.TrimSpace(q) == "" {
		return nil, nil
	}
	var r struct {
		Page struct {
			Media []alMediaT `json:"media"`
		} `json:"Page"`
	}
	err := c.gql(ctx, `query($q: String) { Page(perPage: 10) { media(search: $q, type: MANGA) { ...m } } } `+alMedia,
		map[string]any{"q": q}, &r)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(r.Page.Media))
	for i := range r.Page.Media {
		out = append(out, r.Page.Media[i].entry())
	}
	return out, nil
}

func (c *AniList) Get(ctx context.Context, id string) (*Entry, error) {
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return nil, i18n.Errorf("id de AniList inválido")
	}
	var r struct {
		Media alMediaT `json:"Media"`
	}
	if err := c.gql(ctx, `query($id: Int) { Media(id: $id, type: MANGA) { ...m } } `+alMedia, map[string]any{"id": n}, &r); err != nil {
		return nil, err
	}
	e := r.Media.entry()
	return &e, nil
}

func (c *AniList) List(ctx context.Context, status string) ([]Entry, error) {
	uid, _ := strconv.ParseInt(c.get("user_id"), 10, 64)
	if uid == 0 {
		return nil, ErrNotConnected
	}
	var r struct {
		C struct {
			Lists []struct {
				Entries []struct {
					Media alMediaT `json:"media"`
				} `json:"entries"`
			} `json:"lists"`
		} `json:"MediaListCollection"`
	}
	err := c.gql(ctx, `query($u: Int, $s: MediaListStatus) { MediaListCollection(userId: $u, type: MANGA, status: $s) {
		lists { entries { media { ...m } } } } } `+alMedia, map[string]any{"u": uid, "s": status}, &r)
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, l := range r.C.Lists {
		for i := range l.Entries {
			out = append(out, l.Entries[i].Media.entry())
		}
	}
	return out, nil
}

func (c *AniList) Push(ctx context.Context, id string, read, total int) error {
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return i18n.Errorf("id de AniList inválido")
	}
	return c.gql(ctx, `mutation($id: Int, $p: Int, $s: MediaListStatus) { SaveMediaListEntry(mediaId: $id, progress: $p, status: $s) { id } }`,
		map[string]any{"id": n, "p": read, "s": statusFor(read, total, "PLANNING", "CURRENT", "COMPLETED")}, nil)
}
