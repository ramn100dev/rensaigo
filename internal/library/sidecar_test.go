package library_test

import (
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"rensaigo/internal/library"
	"rensaigo/internal/store"
)

// Una serie escrita con su rensaigo.json se restaura entera en una instalación nueva.
func TestSidecarRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	lib := filepath.Join(tmp, "lib")
	dir := filepath.Join(lib, "Manga", "Serie")
	os.MkdirAll(dir, 0o755)
	img := filepath.Join(tmp, "p.png")
	f, _ := os.Create(img)
	png.Encode(f, image.NewRGBA(image.Rect(0, 0, 4, 4)))
	f.Close()
	for _, n := range []string{"1", "2"} {
		if _, err := library.WriteCBZ(filepath.Join(dir, "[Src][es] Serie "+n+".cbz"), []string{img, img}, &library.ComicInfo{Number: n}); err != nil {
			t.Fatal(err)
		}
	}

	a, err := store.Open(filepath.Join(tmp, "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := library.Scan(a.DB, lib); err != nil {
		t.Fatal(err)
	}
	var sid int64
	a.DB.QueryRow(`SELECT id FROM series`).Scan(&sid)
	a.DB.Exec(`UPDATE series SET title='Título bueno', paused=0, favorite=1 WHERE id=?`, sid)
	a.DB.Exec(`INSERT INTO series_source(series_id,source_id,url,title,priority,from_ch) VALUES(?,?,?,?,0,0),(?,?,?,?,1,5)`,
		sid, "111", "/a", "A", sid, "222", "/b", "B")
	a.DB.Exec(`INSERT INTO tracking(series_id,svc,rid) VALUES(?,'mal','42')`, sid)
	a.DB.Exec(`INSERT INTO progress(chapter_id,page,done,updated) SELECT id,0,1,1 FROM chapter WHERE number=1`)
	a.DB.Exec(`INSERT INTO progress(chapter_id,page,done,updated) SELECT id,1,0,1 FROM chapter WHERE number=2`)
	if err := library.WriteSidecar(a.DB, sid); err != nil {
		t.Fatal(err)
	}
	st1, _ := os.Stat(filepath.Join(dir, library.SidecarName))
	library.WriteSidecar(a.DB, sid) // sin cambios: no se reescribe
	if st2, _ := os.Stat(filepath.Join(dir, library.SidecarName)); !st2.ModTime().Equal(st1.ModTime()) {
		t.Error("se reescribió sin cambios")
	}
	a.Close()

	b, err := store.Open(filepath.Join(tmp, "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := library.Scan(b.DB, lib); err != nil {
		t.Fatal(err)
	}
	var title string
	var paused, fav, imported, nsrc, tmpSrc, ntrk, done, page int
	var from float64
	b.DB.QueryRow(`SELECT title, paused, favorite, imported FROM series`).Scan(&title, &paused, &fav, &imported)
	b.DB.QueryRow(`SELECT count(*), sum(temporary), max(from_ch) FROM series_source`).Scan(&nsrc, &tmpSrc, &from)
	b.DB.QueryRow(`SELECT count(*) FROM tracking WHERE rid='42'`).Scan(&ntrk)
	b.DB.QueryRow(`SELECT p.done FROM progress p JOIN chapter c ON c.id=p.chapter_id WHERE c.number=1`).Scan(&done)
	b.DB.QueryRow(`SELECT p.page FROM progress p JOIN chapter c ON c.id=p.chapter_id WHERE c.number=2`).Scan(&page)
	if title != "Título bueno" || paused != 0 || fav != 1 || imported != 0 || nsrc != 2 || tmpSrc != 1 || from != 5 || ntrk != 1 || done != 1 || page != 1 {
		t.Errorf("restaurado mal: %q paused=%d fav=%d imported=%d srcs=%d temp=%d from=%v trk=%d done=%d page=%d",
			title, paused, fav, imported, nsrc, tmpSrc, from, ntrk, done, page)
	}
}
