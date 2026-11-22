package extrepo

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"
)

const keiyoushi = "9add655a78e96c4ec7a53ef89dccb557cb5d767489fac5e785d671a5a75d4da2"

func TestVerifyJarOK(t *testing.T) {
	fp, err := VerifyJar("testdata/signed.jar", keiyoushi)
	if err != nil || fp != keiyoushi {
		t.Fatalf("jar legítimo rechazado: %v (%s)", err, fp)
	}
}

func TestVerifyJarWrongFingerprint(t *testing.T) {
	if _, err := VerifyJar("testdata/signed.jar", "00"+keiyoushi[2:]); err == nil {
		t.Fatal("se aceptó un firmante distinto del fijado")
	}
}

// rewrite copia el jar aplicando una modificación a cada entrada.
func rewrite(t *testing.T, mod func(name string, data []byte) (string, []byte, bool), extra map[string][]byte) string {
	zr, err := zip.OpenReader("testdata/signed.jar")
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	out := filepath.Join(t.TempDir(), "x.jar")
	f, _ := os.Create(out)
	zw := zip.NewWriter(f)
	for _, e := range zr.File {
		rc, _ := e.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		name, b, keep := mod(e.Name, b)
		if !keep {
			continue
		}
		w, _ := zw.Create(name)
		w.Write(b)
	}
	for n, b := range extra {
		w, _ := zw.Create(n)
		w.Write(b)
	}
	zw.Close()
	f.Close()
	return out
}

func TestVerifyJarTampered(t *testing.T) {
	keep := func(n string, b []byte) (string, []byte, bool) { return n, b, true }
	cases := map[string]string{
		"clase modificada": rewrite(t, func(n string, b []byte) (string, []byte, bool) {
			if filepath.Ext(n) == ".class" && len(b) > 100 {
				b = append([]byte{}, b...)
				b[len(b)-1] ^= 0xff
			}
			return n, b, true
		}, nil),
		"clase añadida": rewrite(t, keep, map[string][]byte{"evil/Evil.class": []byte("x")}),
		"sin firma": rewrite(t, func(n string, b []byte) (string, []byte, bool) {
			return n, b, filepath.Ext(n) != ".RSA" && filepath.Ext(n) != ".SF"
		}, nil),
		"manifiesto modificado": rewrite(t, func(n string, b []byte) (string, []byte, bool) {
			if n == "META-INF/MANIFEST.MF" {
				b = bytes.Replace(b, []byte("Manifest-Version: 1.0"), []byte("Manifest-Version: 1.1"), 1)
			}
			return n, b, true
		}, nil),
	}
	for name, jar := range cases {
		if _, err := VerifyJar(jar, keiyoushi); err == nil {
			t.Errorf("%s: se aceptó un jar manipulado", name)
		} else {
			t.Logf("%s -> rechazado: %v", name, err)
		}
	}
	if _, err := VerifyJar(rewrite(t, keep, nil), keiyoushi); err != nil {
		t.Errorf("reempaquetado sin cambios debería seguir validando: %v", err)
	}
}

func TestParseIndex(t *testing.T) {
	f, err := os.Open("testdata/index.pb")
	if err != nil {
		t.Skip("sin index.pb de prueba")
	}
	zr, _ := gzip.NewReader(f)
	b, _ := io.ReadAll(zr)
	idx, err := ParseIndex(b)
	if err != nil {
		t.Fatal(err)
	}
	if idx.SigningKey != keiyoushi || len(idx.Extensions) < 1000 {
		t.Fatalf("índice inesperado: key=%s n=%d", idx.SigningKey, len(idx.Extensions))
	}
	withJar := 0
	for _, e := range idx.Extensions {
		if e.JarURL != "" && e.Pkg != "" && len(e.Sources) > 0 {
			withJar++
		}
	}
	t.Logf("%d extensiones, %d con jarUrl y sources", len(idx.Extensions), withJar)
}
