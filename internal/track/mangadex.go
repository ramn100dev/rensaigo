package track

// MangaDex. Conexión con un «cliente personal» (https://mangadex.org/settings → API Clients): el usuario pega aquí
// el client_id y el secreto y su usuario y contraseña de MangaDex. La contraseña solo se usa para pedir el primer
// token y no se guarda; después se renueva con el refresh token.
//
// MangaDex no lleva un contador de capítulos leídos: guarda marcas de lectura por capítulo (cada traducción es un
// capítulo distinto). Para leer y escribir «llevo N leídos» se usa /aggregate, que agrupa por número de capítulo
// todos sus ids.

import (
	"bytes"
	"context"
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
	mdAPI      = "https://api.mangadex.org"
	mdTokenURL = "https://auth.mangadex.org/realms/mangadex/protocol/openid-connect/token"
	mdUA       = "rensaigo"
)

type MangaDex struct {
	kv
	HTTP *http.Client
	Lang func() string // idioma preferido para la sinopsis
	mu   sync.Mutex
	g    gate
}

func NewMangaDex(st *store.Store, lang func() string) *MangaDex {
	return &MangaDex{kv: kv{st, "mangadex"}, HTTP: &http.Client{Timeout: 30 * time.Second}, Lang: lang,
		g: gate{every: 250 * time.Millisecond}} // límite general: 5 peticiones por segundo
}

func (c *MangaDex) Key() string      { return "mangadex" }
func (c *MangaDex) Name() string     { return "MangaDex" }
func (c *MangaDex) User() string     { return c.get("user") }
func (c *MangaDex) Connected() bool  { return c.get("refresh") != "" }
func (c *MangaDex) ClientID() string { return c.get("client_id") }
func (c *MangaDex) Statuses() []Status {
	return []Status{{"plan_to_read", "Pendientes"}, {"reading", "Leyendo"}, {"on_hold", "En pausa"}}
}

func (c *MangaDex) Disconnect() { c.clear("access", "refresh", "expires", "user") }

// Login pide los tokens con usuario y contraseña (grant «password» del cliente personal).
func (c *MangaDex) Login(ctx context.Context, user, pass, id, secret string) error {
	c.setCredentials(id, secret) // secreto vacío con el mismo ID: el guardado
	id, secret = c.get("client_id"), c.get("client_secret")
	if strings.TrimSpace(user) == "" || pass == "" || id == "" || secret == "" {
		return i18n.Errorf("rellena los cuatro campos")
	}
	f := url.Values{"grant_type": {"password"}, "username": {strings.TrimSpace(user)}, "password": {pass},
		"client_id": {id}, "client_secret": {secret}}
	if err := c.tokenRequest(ctx, f); err != nil {
		return err
	}
	var me struct {
		Data struct {
			Attributes struct {
				Username string `json:"username"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := c.do(ctx, "GET", "/user/me", nil, nil, &me); err != nil {
		c.Disconnect()
		return err
	}
	c.set("user", me.Data.Attributes.Username)
	return nil
}

func (c *MangaDex) tokenRequest(ctx context.Context, f url.Values) error {
	req, _ := http.NewRequestWithContext(ctx, "POST", mdTokenURL, strings.NewReader(f.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", mdUA)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		var e struct {
			Err  string `json:"error"`
			Desc string `json:"error_description"`
		}
		json.Unmarshal(b, &e)
		switch {
		case e.Err == "invalid_grant" && f.Get("grant_type") == "password":
			return i18n.Errorf("MangaDex no acepta ese usuario y contraseña")
		case e.Err == "invalid_grant":
			return ErrSessionExpired
		case e.Err == "unauthorized_client" || e.Err == "invalid_client":
			return i18n.Errorf("MangaDex no acepta ese cliente: revisa el client_id y el secreto, y que el cliente esté aprobado (en su página de ajustes)")
		}
		return i18n.Errorf("MangaDex rechazó el token (%d): %s %s", resp.StatusCode, e.Err, e.Desc)
	}
	var t struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		Expires int64  `json:"expires_in"`
	}
	if json.Unmarshal(b, &t) != nil || t.Access == "" {
		return i18n.Errorf("respuesta de token de MangaDex inválida")
	}
	c.set("access", t.Access)
	if t.Refresh != "" {
		c.set("refresh", t.Refresh)
	}
	if t.Expires == 0 {
		t.Expires = 900
	}
	c.set("expires", strconv.FormatInt(time.Now().Unix()+t.Expires, 10))
	return nil
}

// token: el access token dura 15 minutos; se renueva con un minuto de margen.
func (c *MangaDex) token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.Connected() {
		return "", ErrNotConnected
	}
	exp, _ := strconv.ParseInt(c.get("expires"), 10, 64)
	if time.Now().Unix() < exp-60 {
		return c.get("access"), nil
	}
	f := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {c.get("refresh")},
		"client_id": {c.get("client_id")}, "client_secret": {c.get("client_secret")}}
	if err := c.tokenRequest(ctx, f); err != nil {
		if errors.Is(err, ErrSessionExpired) {
			c.Disconnect()
		}
		return "", err
	}
	return c.get("access"), nil
}

// do llama a la API. auth=false para lo público (búsqueda, fichas, aggregate): así funciona aunque la sesión falle.
func (c *MangaDex) do(ctx context.Context, method, path string, q url.Values, body any, out any) error {
	return c.call(ctx, true, method, path, q, body, out)
}

func (c *MangaDex) call(ctx context.Context, auth bool, method, path string, q url.Values, body any, out any) error {
	u := mdAPI + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequestWithContext(ctx, method, u, rd)
	req.Header.Set("User-Agent", mdUA)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth {
		tok, err := c.token(ctx)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if err := c.g.wait(ctx); err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	switch {
	case resp.StatusCode == 401:
		return i18n.Errorf("MangaDex ha rechazado la sesión: vuelve a conectar desde Ajustes")
	case resp.StatusCode == 429:
		return i18n.Errorf("MangaDex pide ir más despacio: prueba en un minuto")
	case resp.StatusCode/100 != 2:
		var e struct {
			Errors []struct {
				Detail string `json:"detail"`
			} `json:"errors"`
		}
		json.Unmarshal(b, &e)
		if len(e.Errors) > 0 {
			return i18n.Errorf("MangaDex respondió %d: %s", resp.StatusCode, e.Errors[0].Detail)
		}
		return i18n.Errorf("MangaDex respondió %d", resp.StatusCode)
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

// ---------------------------------------------------------------- fichas

type mdManga struct {
	ID         string `json:"id"`
	Attributes struct {
		Title       map[string]string   `json:"title"`
		AltTitles   []map[string]string `json:"altTitles"`
		Description map[string]string   `json:"description"`
		Status      string              `json:"status"`
		LastChapter string              `json:"lastChapter"`
		Tags        []struct {
			Attributes struct {
				Name  map[string]string `json:"name"`
				Group string            `json:"group"`
			} `json:"attributes"`
		} `json:"tags"`
	} `json:"attributes"`
	Relationships []struct {
		Type       string `json:"type"`
		Attributes *struct {
			Name     string `json:"name"`
			FileName string `json:"fileName"`
		} `json:"attributes"`
	} `json:"relationships"`
}

var mdContent = []string{"safe", "suggestive", "erotica", "pornographic"}

func mdIncludes(q url.Values) url.Values {
	for _, i := range []string{"cover_art", "author", "artist"} {
		q.Add("includes[]", i)
	}
	for _, r := range mdContent {
		q.Add("contentRating[]", r)
	}
	return q
}

func (c *MangaDex) entry(m *mdManga) Entry {
	a := m.Attributes
	pick := func(mp map[string]string, langs ...string) string {
		for _, l := range langs {
			if v := mp[l]; v != "" {
				return v
			}
		}
		for _, v := range mp {
			return v
		}
		return ""
	}
	e := Entry{ID: m.ID, Title: pick(a.Title, "en", "ja-ro", "ko-ro", "zh-ro", "ja"), URL: "https://mangadex.org/title/" + m.ID}
	for _, t := range a.AltTitles {
		for l, v := range t {
			if l == "en" && e.En == "" && v != e.Title {
				e.En = v
			} else {
				e.Alt = append(e.Alt, v)
			}
		}
	}
	lang := "es"
	if c.Lang != nil {
		lang = c.Lang()
	}
	e.Synopsis = plain(pick(a.Description, lang, strings.ToLower(lang), strings.SplitN(lang, "-", 2)[0], "en"))
	var g, au []string
	for _, t := range a.Tags {
		if t.Attributes.Group == "genre" || t.Attributes.Group == "theme" {
			g = append(g, pick(t.Attributes.Name, "en"))
		}
	}
	seen := map[string]bool{}
	for _, r := range m.Relationships {
		if r.Attributes == nil {
			continue
		}
		switch r.Type {
		case "cover_art":
			if r.Attributes.FileName != "" {
				base := "https://uploads.mangadex.org/covers/" + m.ID + "/" + r.Attributes.FileName
				e.Pic, e.PicSmall = base+".512.jpg", base+".256.jpg"
			}
		case "author", "artist":
			if n := r.Attributes.Name; n != "" && !seen[n] {
				seen[n] = true
				au = append(au, n)
			}
		}
	}
	e.Genres, e.Authors = strings.Join(g, ", "), strings.Join(au, ", ")
	switch a.Status {
	case "completed":
		e.Pub = 2
		if n, err := strconv.ParseFloat(a.LastChapter, 64); err == nil {
			e.Total = int(n)
		}
	case "hiatus":
		e.Pub = 6
	case "cancelled":
		e.Pub = 5
	case "ongoing":
		e.Pub = 1
	}
	return e
}

func (c *MangaDex) Search(ctx context.Context, q string) ([]Entry, error) {
	if strings.TrimSpace(q) == "" {
		return nil, nil
	}
	var r struct {
		Data []mdManga `json:"data"`
	}
	v := mdIncludes(url.Values{"title": {q}, "limit": {"10"}, "order[relevance]": {"desc"}})
	if err := c.call(ctx, false, "GET", "/manga", v, nil, &r); err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(r.Data))
	for i := range r.Data {
		out = append(out, c.entry(&r.Data[i]))
	}
	return out, nil
}

// Get trae la ficha y, si hay sesión, lo leído: el número de capítulo más alto con marca de lectura.
func (c *MangaDex) Get(ctx context.Context, id string) (*Entry, error) {
	var r struct {
		Data mdManga `json:"data"`
	}
	if err := c.call(ctx, false, "GET", "/manga/"+url.PathEscape(id), mdIncludes(url.Values{}), nil, &r); err != nil {
		return nil, err
	}
	e := c.entry(&r.Data)
	if c.Connected() {
		var st struct {
			Status string `json:"status"`
		}
		if c.do(ctx, "GET", "/manga/"+url.PathEscape(id)+"/status", nil, nil, &st) == nil {
			e.MyStatus = st.Status
		}
		var rd struct {
			Data []string `json:"data"`
		}
		if err := c.do(ctx, "GET", "/manga/"+url.PathEscape(id)+"/read", nil, nil, &rd); err != nil {
			return nil, err
		}
		if len(rd.Data) > 0 {
			nums, err := c.aggregate(ctx, id)
			if err != nil {
				return nil, err
			}
			for _, chID := range rd.Data {
				if n, ok := nums[chID]; ok && int(n) > e.MyRead {
					e.MyRead = int(n)
				}
			}
		}
	}
	return &e, nil
}

// aggregate: id de capítulo (de cualquier traducción) → número de capítulo.
func (c *MangaDex) aggregate(ctx context.Context, id string) (map[string]float64, error) {
	var r struct {
		Volumes json.RawMessage `json:"volumes"` // objeto, o [] si no hay ninguno
	}
	if err := c.call(ctx, false, "GET", "/manga/"+url.PathEscape(id)+"/aggregate", nil, nil, &r); err != nil {
		return nil, err
	}
	var vols map[string]struct {
		Chapters json.RawMessage `json:"chapters"`
	}
	json.Unmarshal(r.Volumes, &vols)
	out := map[string]float64{}
	for _, v := range vols {
		var chs map[string]struct {
			Chapter string   `json:"chapter"`
			ID      string   `json:"id"`
			Others  []string `json:"others"`
		}
		json.Unmarshal(v.Chapters, &chs)
		for _, ch := range chs {
			n, err := strconv.ParseFloat(ch.Chapter, 64)
			if err != nil {
				continue // «none»: capítulos sin número (oneshots)
			}
			out[ch.ID] = n
			for _, o := range ch.Others {
				out[o] = n
			}
		}
	}
	return out, nil
}

func (c *MangaDex) List(ctx context.Context, status string) ([]Entry, error) {
	var st struct {
		Statuses map[string]string `json:"statuses"`
	}
	if err := c.do(ctx, "GET", "/manga/status", url.Values{"status": {status}}, nil, &st); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(st.Statuses))
	for id := range st.Statuses {
		ids = append(ids, id)
	}
	var out []Entry
	for len(ids) > 0 {
		n := min(len(ids), 100)
		q := mdIncludes(url.Values{"limit": {"100"}})
		for _, id := range ids[:n] {
			q.Add("ids[]", id)
		}
		ids = ids[n:]
		var r struct {
			Data []mdManga `json:"data"`
		}
		if err := c.call(ctx, false, "GET", "/manga", q, nil, &r); err != nil {
			return nil, err
		}
		for i := range r.Data {
			e := c.entry(&r.Data[i])
			e.MyStatus = status
			out = append(out, e)
		}
	}
	return out, nil
}

// Push deja marcados como leídos los capítulos hasta el read (todas sus traducciones) y como no leídos los de después,
// y pone el estado de la lista. Solo manda lo que cambia respecto a las marcas que ya hay: en una serie larga con
// muchas traducciones son miles de ids, y la mayoría ya están bien.
func (c *MangaDex) Push(ctx context.Context, id string, read, total int) error {
	nums, err := c.aggregate(ctx, id)
	if err != nil {
		return err
	}
	var cur struct {
		Data []string `json:"data"`
	}
	if err := c.do(ctx, "GET", "/manga/"+url.PathEscape(id)+"/read", nil, nil, &cur); err != nil {
		return err
	}
	marked := make(map[string]bool, len(cur.Data))
	for _, ch := range cur.Data {
		marked[ch] = true
	}
	var rd, un []string
	for ch, n := range nums {
		switch want := n > 0 && int(n) <= read; {
		case want && !marked[ch]:
			rd = append(rd, ch)
		case !want && marked[ch] && int(n) > read:
			un = append(un, ch)
		}
	}
	// el cuerpo admite como mucho 10 KB: unos 200 ids por petición
	for len(rd) > 0 || len(un) > 0 {
		body := map[string][]string{}
		take := func(xs *[]string, key string, room int) int {
			n := min(len(*xs), room)
			if n > 0 {
				body[key] = (*xs)[:n]
				*xs = (*xs)[n:]
			}
			return n
		}
		room := 200 - take(&rd, "chapterIdsRead", 200)
		take(&un, "chapterIdsUnread", room)
		if err := c.do(ctx, "POST", "/manga/"+url.PathEscape(id)+"/read", nil, body, nil); err != nil {
			return err
		}
	}
	return c.do(ctx, "POST", "/manga/"+url.PathEscape(id)+"/status", nil,
		map[string]string{"status": statusFor(read, total, "plan_to_read", "reading", "completed")}, nil)
}
