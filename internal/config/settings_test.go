package config

import (
	"path/filepath"
	"testing"
	"time"
)

func TestSettingsApply(t *testing.T) {
	c := &Config{}
	ok := map[string][2]string{ // clave → valor escrito, valor guardado
		"check_interval":  {" 90m ", "1h30m"},
		"rate_interval":   {"0s", "0s"},
		"dl_concurrency":  {"4", "4"},
		"ui_lang":         {"es", "es"},
		"backup_interval": {"24h", "24h"},
	}
	for k, v := range ok {
		got, err := FindSetting(k).Apply(c, v[0])
		if err != nil || got != v[1] {
			t.Errorf("%s=%q → %q, %v (esperado %q)", k, v[0], got, err, v[1])
		}
	}
	if c.CheckInterval != 90*time.Minute || c.DLConcurrency != 4 || c.UILang != "es" {
		t.Errorf("no se aplicó: %+v", c)
	}
	bad := map[string]string{
		"check_interval": "5m",   // por debajo del mínimo (10 min)
		"rate_interval":  "-1s",
		"dl_concurrency": "0",
		"per_source":     "dos",
		"ui_lang":        "fr",
		"lang":           "",
		"download_path":  "relativa/Manga",
	}
	for k, v := range bad {
		if _, err := FindSetting(k).Apply(c, v); err == nil {
			t.Errorf("%s=%q se aceptó", k, v)
		}
	}
	if c.CheckInterval != 90*time.Minute || c.DLConcurrency != 4 {
		t.Error("un valor rechazado cambió la configuración")
	}
	// la carpeta de descarga se parte en biblioteca + categoría
	dir := filepath.Join(t.TempDir(), "mangas", "Manga")
	if _, err := FindSetting("download_path").Apply(c, dir); err != nil {
		t.Fatal(err)
	}
	if c.LibraryDir != filepath.Dir(dir) || c.DefaultCat != "Manga" {
		t.Errorf("biblioteca %q, categoría %q", c.LibraryDir, c.DefaultCat)
	}
	for _, s := range Settings { // todos los ajustes devuelven su valor sin romperse
		_ = s.Value(c)
	}
}
