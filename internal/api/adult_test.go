package api

import "testing"

func TestAdultRe(t *testing.T) {
	for name, want := range map[string]bool{
		"Shadow Manga (+18)": true, "Foo 18+": true, "Bar (NSFW)": true, "Baz Hentai": true,
		"Shadow Manga": false, "MangaOni": false, "Manga18Fx": false, "Adultos": true,
	} {
		if adultRe.MatchString(name) != want {
			t.Errorf("%q: esperaba %v", name, want)
		}
	}
}
