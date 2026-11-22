package track

import (
	"encoding/json"
	"net/url"
	"path/filepath"
	"testing"

	"rensaigo/internal/store"
)

func newStore(t *testing.T) *store.Store {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestMALAuthURL(t *testing.T) {
	st := newStore(t)
	c := NewMAL(st)
	if _, err := c.AuthURL("http://x/mal/callback"); err == nil {
		t.Fatal("sin Client ID debería fallar")
	}
	c.SetCredentials(" abc123 ", "")
	u, err := c.AuthURL("http://192.168.1.10:9840/mal/callback")
	if err != nil {
		t.Fatal(err)
	}
	p, _ := url.Parse(u)
	q := p.Query()
	if q.Get("client_id") != "abc123" || q.Get("code_challenge_method") != "plain" || q.Get("redirect_uri") != "http://192.168.1.10:9840/mal/callback" {
		t.Fatalf("parámetros: %v", q)
	}
	if v := st.Get("mal.verifier", ""); q.Get("code_challenge") != v || len(v) < 43 || len(v) > 128 {
		t.Fatalf("verificador %q", v)
	}
	if c.Connected() {
		t.Fatal("no debería estar conectado")
	}
}

func TestAniListAuthNeedsSecret(t *testing.T) {
	c := NewAniList(newStore(t))
	c.SetCredentials("1234", "")
	if _, err := c.AuthURL("http://x/anilist/callback"); err == nil {
		t.Fatal("sin secreto debería fallar")
	}
	c.SetCredentials("1234", "s3cr3t")
	u, err := c.AuthURL("http://x/anilist/callback")
	if err != nil {
		t.Fatal(err)
	}
	p, _ := url.Parse(u)
	if p.Query().Get("response_type") != "code" || p.Query().Get("client_id") != "1234" {
		t.Fatalf("url %s", u)
	}
}

func TestMALEntry(t *testing.T) {
	var m malManga
	json.Unmarshal([]byte(`{"id":642,"title":"Vinland Saga","alternative_titles":{"en":"Vinland Saga","ja":"ヴィンランド・サガ","synonyms":[]},
		"status":"finished","num_chapters":224,"authors":[{"node":{"first_name":"Makoto","last_name":"Yukimura"}}],
		"genres":[{"name":"Action"},{"name":"Drama"}],"my_list_status":{"status":"reading","num_chapters_read":209}}`), &m)
	e := m.entry()
	if e.ID != "642" || e.Pub != 2 || e.Total != 224 || e.MyRead != 209 || e.Authors != "Makoto Yukimura" || e.Genres != "Action, Drama" {
		t.Fatalf("%+v", e)
	}
	if got := e.Titles(); len(got) != 2 || got[1] != "ヴィンランド・サガ" {
		t.Fatalf("Titles: %v", got)
	}
}

func TestAniListEntry(t *testing.T) {
	var m alMediaT
	json.Unmarshal([]byte(`{"id":30642,"title":{"romaji":"Vinland Saga","english":"Vinland Saga","native":"ヴィンランド・サガ"},
		"status":"RELEASING","chapters":0,"description":"Thorfinn<br>sails &amp; fights.","genres":["Action"],
		"staff":{"edges":[{"role":"Story & Art","node":{"name":{"full":"Makoto Yukimura"}}},{"role":"Translator (English)","node":{"name":{"full":"X"}}}]},
		"mediaListEntry":{"progress":12,"status":"CURRENT"}}`), &m)
	e := m.entry()
	if e.Pub != 1 || e.Authors != "Makoto Yukimura" || e.MyRead != 12 || e.Synopsis != "Thorfinn\nsails & fights." {
		t.Fatalf("%+v", e)
	}
}

func TestMangaDexEntry(t *testing.T) {
	var m mdManga
	json.Unmarshal([]byte(`{"id":"5d1f","attributes":{"title":{"ja-ro":"Vinland Saga"},"altTitles":[{"ja":"ヴィンランド・サガ"},{"en":"Vinland Saga EN"}],
		"description":{"en":"Eng","es":"Esp"},"status":"completed","lastChapter":"220",
		"tags":[{"attributes":{"name":{"en":"Action"},"group":"genre"}},{"attributes":{"name":{"en":"Award Winning"},"group":"format"}}]},
		"relationships":[{"type":"author","attributes":{"name":"Yukimura Makoto"}},{"type":"artist","attributes":{"name":"Yukimura Makoto"}},
		{"type":"cover_art","attributes":{"fileName":"c.jpg"}},{"type":"manga"}]}`), &m)
	c := NewMangaDex(newStore(t), func() string { return "es" })
	e := c.entry(&m)
	if e.Title != "Vinland Saga" || e.En != "Vinland Saga EN" || e.Synopsis != "Esp" || e.Total != 220 || e.Pub != 2 ||
		e.Genres != "Action" || e.Authors != "Yukimura Makoto" || e.Pic != "https://uploads.mangadex.org/covers/5d1f/c.jpg.512.jpg" {
		t.Fatalf("%+v", e)
	}
}

func TestStatusFor(t *testing.T) {
	for _, c := range []struct {
		read, total int
		want        string
	}{{0, 10, "p"}, {3, 10, "r"}, {10, 10, "c"}, {12, 0, "r"}} {
		if got := statusFor(c.read, c.total, "p", "r", "c"); got != c.want {
			t.Errorf("%d/%d: %s", c.read, c.total, got)
		}
	}
}
