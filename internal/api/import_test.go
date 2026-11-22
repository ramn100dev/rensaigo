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
