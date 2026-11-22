package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSecrets(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.Set("mal.access", "tok-antes") // guardado antes de activar el cifrado: en claro
	st.Set("cfg.lang", "es")
	if err := st.EnableSecrets(filepath.Join(dir, "secret.key")); err != nil {
		t.Fatal(err)
	}
	raw := func(k string) (v string) { st.DB.QueryRow(`SELECT value FROM setting WHERE key=?`, k).Scan(&v); return }
	if r := raw("mal.access"); !strings.HasPrefix(r, encPrefix) || strings.Contains(r, "tok-antes") {
		t.Fatalf("no se cifró el existente: %q", r)
	}
	if raw("cfg.lang") != "es" {
		t.Fatal("un ajuste normal no debe cifrarse")
	}
	if st.Get("mal.access", "") != "tok-antes" {
		t.Fatal("no se descifra")
	}
	st.Set("notify.channels", `[{"token":"x"}]`)
	if strings.Contains(raw("notify.channels"), "token") || st.Get("notify.channels", "") != `[{"token":"x"}]` {
		t.Fatal("ida y vuelta de notify.channels")
	}
	if fi, _ := os.Stat(filepath.Join(dir, "secret.key")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("permisos de la clave: %v", fi.Mode())
	}
	// con otra clave no se puede leer: devuelve el valor por defecto (hay que volver a conectar)
	os.WriteFile(filepath.Join(dir, "otra.key"), make([]byte, 32), 0o600)
	if err := st.EnableSecrets(filepath.Join(dir, "otra.key")); err != nil {
		t.Fatal(err)
	}
	if st.Get("mal.access", "nada") != "nada" {
		t.Fatal("con otra clave no debería descifrar")
	}
}
