package api

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
)

// Emparejar una serie adoptada del disco: con «todos» enseña candidatos; con un source y un título claramente
// igual la enlaza sola, la reanuda y comprueba sus capítulos (los que ya estaban en disco no se duplican).
func TestImportAuto(t *testing.T) {
	env := newTestEnv(t)
	path := fmt.Sprintf("/import/%d/auto", env.imp)
	w := env.do(t, "POST", path, url.Values{"source": {"all"}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Old One") {
		t.Fatalf("candidatos: %d %s", w.Code, w.Body.String())
	}
	w = env.do(t, "POST", path, url.Values{"source": {"B"}})
	if w.Code != 200 {
		t.Fatalf("emparejar: %d %s", w.Code, w.Body.String())
	}
	var nss, paused, auto, chapters, ch1 int
	env.s.St.DB.QueryRow(`SELECT count(*) FROM series_source WHERE series_id=?`, env.imp).Scan(&nss)
	env.s.St.DB.QueryRow(`SELECT paused, auto_download FROM series WHERE id=?`, env.imp).Scan(&paused, &auto)
	env.s.St.DB.QueryRow(`SELECT count(*), sum(number=1) FROM chapter WHERE series_id=?`, env.imp).Scan(&chapters, &ch1)
	if nss != 1 || paused != 0 || auto != 1 {
		t.Errorf("tras emparejar: sources %d, pausada %d, auto %d", nss, paused, auto)
	}
	if chapters != 3 || ch1 != 1 {
		t.Errorf("capítulos tras comprobar: %d (el 1, %d veces)", chapters, ch1)
	}
	// un título que no se parece a nada no se enlaza a ciegas
	env.s.St.DB.Exec(`DELETE FROM series_source WHERE series_id=?`, env.imp)
	env.s.St.DB.Exec(`UPDATE series SET title='Nothing Alike' WHERE id=?`, env.imp)
	w = env.do(t, "POST", path, url.Values{"source": {"B"}})
	var n int
	env.s.St.DB.QueryRow(`SELECT count(*) FROM series_source WHERE series_id=?`, env.imp).Scan(&n)
	if n != 0 {
		t.Errorf("se enlazó con un título distinto: %s", w.Body.String())
	}
}

// Al emparejar una serie importada, lo que el source tiene por debajo del capítulo más alto en disco se queda
// «disponible» (una serie con tomos sueltos no se pone a bajar cientos de capítulos viejos); lo posterior se pone
// en cola. Con «from», lo anterior ni aparece.
func TestImportLinkKeepsPastAvailable(t *testing.T) {
	env := newTestEnv(t)
	db := env.s.St.DB
	db.Exec(`UPDATE chapter SET number=2 WHERE series_id=?`, env.imp) // en disco: el 2
	w := env.do(t, "POST", fmt.Sprintf("/import/%d/match", env.imp), url.Values{"source": {"B"}, "url": {"/old"}, "title": {"Old One"}, "auto": {"1"}})
	if w.Code != 200 {
		t.Fatalf("emparejar: %d %s", w.Code, w.Body.String())
	}
	state := func(n int) (s string) {
		db.QueryRow(`SELECT state FROM chapter WHERE series_id=? AND number=?`, env.imp, n).Scan(&s)
		return
	}
	if state(1) != "available" || state(2) != "downloaded" || state(3) == "available" || state(3) == "" {
		t.Errorf("estados tras emparejar: 1=%s 2=%s 3=%s", state(1), state(2), state(3))
	}
	var paused, auto int
	db.QueryRow(`SELECT paused, auto_download FROM series WHERE id=?`, env.imp).Scan(&paused, &auto)
	if paused != 0 || auto != 1 {
		t.Errorf("pausada %d, auto %d", paused, auto)
	}

	// el primer source de una serie importada añadido con «follow» + add_to: igual, y no se queda en pausa
	env = newTestEnv(t)
	db = env.s.St.DB
	w = env.do(t, "POST", "/series/follow", url.Values{"source": {"B"}, "url": {"/old"}, "title": {"Old One"}, "add_to": {fmt.Sprint(env.imp)}, "from": {"3"}})
	if w.Code >= 400 {
		t.Fatalf("follow add_to: %d %s", w.Code, w.Body.String())
	}
	db.QueryRow(`SELECT paused FROM series WHERE id=?`, env.imp).Scan(&paused)
	var below int
	db.QueryRow(`SELECT count(*) FROM chapter WHERE series_id=? AND number=2`, env.imp).Scan(&below)
	if paused != 0 || below != 0 {
		t.Errorf("add_to: pausada %d, capítulos por debajo de «from» %d", paused, below)
	}
}
