package library_test

import (
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"rensaigo/internal/library"
	"rensaigo/internal/store"
)

// Con títulos larguísimos el nombre se recorta, pero nunca el número: dos capítulos no comparten archivo.
func TestChapterFileLong(t *testing.T) {
	s := "Tensei Shitara Slime Datta Ken: The Ultimate Reincarnated Slime Story Spin-off Edition"
	a := library.ChapterFile("Weeb Central", "en", s, s+" Chapter 12.5", 12.5)
	b := library.ChapterFile("Weeb Central", "en", s, s+" Chapter 13", 13)
	if a == b || !strings.HasSuffix(a, " 12.5.cbz") || !strings.HasSuffix(b, " 13.cbz") {
		t.Fatalf("nombres: %q %q", a, b)
	}
	if got := library.ChapterFile("Src", "es", "Serie", "", 3); got != "[Src][es] Serie (Capítulo 3) 03.cbz" {
		t.Fatalf("nombre corto cambiado: %q", got)
	}
	j := library.SafeName(strings.Repeat("転生したらスライムだった件 ", 20))
	if !utf8.ValidString(j) || len(j) > 150 {
		t.Fatalf("recorte no válido: %q", j)
	}
}

// Si el disco de la biblioteca no está (montaje caído), el escaneo no da todo por desaparecido.
func TestScanOffline(t *testing.T) {
	tmp := t.TempDir()
	lib := filepath.Join(tmp, "lib")
	dir := filepath.Join(lib, "Manga", "Serie")
	os.MkdirAll(dir, 0o755)
	img := filepath.Join(tmp, "p.png")
	f, _ := os.Create(img)
	png.Encode(f, image.NewRGBA(image.Rect(0, 0, 4, 4)))
	f.Close()
	for i := 1; i <= 12; i++ {
		n := string(rune('0'+i/10)) + string(rune('0'+i%10))
		if _, err := library.WriteCBZ(filepath.Join(dir, "[Src][es] Serie "+n+".cbz"), []string{img}, &library.ComicInfo{}); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(filepath.Join(tmp, "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := library.Scan(st.DB, lib); err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(lib)
	os.MkdirAll(lib, 0o755) // punto de montaje vacío
	rep, err := library.Scan(st.DB, lib)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	st.DB.QueryRow(`SELECT count(*) FROM chapter WHERE state='downloaded'`).Scan(&n)
	if !rep.Offline || n != 12 {
		t.Fatalf("offline=%v, descargados=%d (esperado 12)", rep.Offline, n)
	}
	// borrar unos pocos a mano sí cuenta
	os.RemoveAll(lib)
	os.MkdirAll(dir, 0o755)
	library.WriteCBZ(filepath.Join(dir, "x.cbz"), []string{img}, &library.ComicInfo{})
	for i := 1; i <= 10; i++ {
		n := string(rune('0'+i/10)) + string(rune('0'+i%10))
		library.WriteCBZ(filepath.Join(dir, "[Src][es] Serie "+n+".cbz"), []string{img}, &library.ComicInfo{})
	}
	rep, _ = library.Scan(st.DB, lib)
	if rep.Offline || rep.MissingOnDisk != 2 {
		t.Fatalf("offline=%v, desaparecidos=%d (esperado 2)", rep.Offline, rep.MissingOnDisk)
	}
}
