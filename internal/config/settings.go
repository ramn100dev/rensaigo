package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Setting es un ajuste editable desde la web. Se guarda en la BD (clave "cfg.<Key>") y manda sobre la variable de
// entorno, que queda como valor por defecto. Los componentes leen Config en cada uso, así que el cambio es inmediato.
type Setting struct {
	Key, Group, Label, Help, Kind string // Kind: dur | int | text | choice | path
	Min                           float64
	Options                       []string                              // Kind choice
	get                           func(*Config) string                  // ajustes que no son un solo campo
	set                           func(*Config, string) (string, error) // (p. ej. la ruta de descarga)
	ptrD                          func(*Config) *time.Duration
	ptrI                          func(*Config) *int
	ptrS                          func(*Config) *string
}

// Langs: idiomas que se ofrecen como preferido (los más comunes en el repositorio de extensiones).
var Langs = []string{"es", "es-419", "en", "pt-BR", "fr", "it", "de", "ja", "all"}

var Settings = []Setting{
	{Key: "lang", Group: "General", Label: "Idioma preferido", Kind: "choice", Options: Langs,
		Help: "Idioma con el que se filtran de entrada las extensiones disponibles en Sources.", ptrS: func(c *Config) *string { return &c.PrefLang }},
	{Key: "adult", Group: "General", Label: "Contenido +18", Kind: "choice", Options: []string{"no", "sí"},
		Help: "Con «no», las extensiones +18 no salen en Disponibles. También es lo que usan por defecto las que traen una versión +18 aparte (como Shadow Manga); eso se cambia en los ajustes de cada una.",
		ptrS: func(c *Config) *string { return &c.Adult }},
	{Key: "download_path", Group: "General", Label: "Carpeta de descarga", Kind: "path",
		Help: "Ruta completa donde se guardan las series (cada una en su carpeta). Las series que ya haya ahí dentro se adoptan solas. En Docker la ruta tiene que existir DENTRO del contenedor: monta tu carpeta como volumen (p. ej. -v /mnt/discos/mangas:/mnt/discos/mangas) y escribe aquí esa misma ruta.",
		get:  func(c *Config) string { return filepath.Join(c.LibraryDir, c.DefaultCat) }, set: setDownloadPath},
	{Key: "check_interval", Group: "General", Label: "Buscar capítulos nuevos cada", Kind: "dur", Min: 600,
		Help: "Cada cuánto se consulta el source de cada serie activa. Ej.: 2h, 90m.", ptrD: func(c *Config) *time.Duration { return &c.CheckInterval }},
	{Key: "max_attempts", Group: "General", Label: "Intentos por capítulo", Kind: "int", Min: 1,
		Help: "Reintentos con espera creciente (30 s, 2 min, 8 min…) antes de marcarlo como fallido.", ptrI: func(c *Config) *int { return &c.MaxAttempts }},
	{Key: "dl_concurrency", Group: "Descargas", Label: "Capítulos a la vez", Kind: "int", Min: 1,
		Help: "Descargas simultáneas en total.", ptrI: func(c *Config) *int { return &c.DLConcurrency }},
	{Key: "per_source", Group: "Descargas", Label: "Capítulos a la vez por source", Kind: "int", Min: 1,
		Help: "Súbelo con cuidado: muchas webs bloquean a quien pide demasiado.", ptrI: func(c *Config) *int { return &c.PerSource }},
	{Key: "page_concurrency", Group: "Descargas", Label: "Imágenes a la vez por capítulo", Kind: "int", Min: 1,
		Help: "Páginas que se bajan en paralelo dentro de un capítulo.", ptrI: func(c *Config) *int { return &c.PageConcurrency }},
	{Key: "rate_interval", Group: "Descargas", Label: "Pausa entre peticiones a un source", Kind: "dur", Min: 0,
		Help: "Separación mínima entre dos peticiones al mismo sitio. Ej.: 200ms, 1s.", ptrD: func(c *Config) *time.Duration { return &c.RateInterval }},
	{Key: "ext_update", Group: "Mantenimiento", Label: "Actualizar extensiones cada", Kind: "dur", Min: 3600,
		Help: "Índice del repositorio y extensiones instaladas. Se aplica tras la siguiente actualización.", ptrD: func(c *Config) *time.Duration { return &c.ExtUpdate }},
	{Key: "backup_interval", Group: "Mantenimiento", Label: "Copia de la BD cada", Kind: "dur", Min: 3600,
		Help: "Copia compacta de la base de datos (no de la biblioteca).", ptrD: func(c *Config) *time.Duration { return &c.BackupInterval }},
	{Key: "backup_keep", Group: "Mantenimiento", Label: "Copias que se conservan", Kind: "int", Min: 1,
		Help: "Las más antiguas se borran.", ptrI: func(c *Config) *int { return &c.BackupKeep }},
	{Key: "thumb_cache_mb", Group: "Mantenimiento", Label: "Caché de portadas (MB)", Kind: "int", Min: 8,
		Help: "Miniaturas generadas desde los CBZ. Al llenarse se borran las menos usadas.", ptrI: func(c *Config) *int { return &c.ThumbCacheMB }},
}

func FindSetting(key string) *Setting {
	for i := range Settings {
		if Settings[i].Key == key {
			return &Settings[i]
		}
	}
	return nil
}

// Value devuelve el valor actual en el formato que se edita.
func (s *Setting) Value(c *Config) string {
	if s.get != nil {
		return s.get(c)
	}
	switch s.Kind {
	case "dur":
		return fmtDur(*s.ptrD(c))
	case "int":
		return strconv.Itoa(*s.ptrI(c))
	}
	return *s.ptrS(c)
}

// Apply valida y aplica un valor. Devuelve el valor normalizado, que es el que se guarda.
func (s *Setting) Apply(c *Config, v string) (string, error) {
	v = strings.TrimSpace(v)
	if s.set != nil {
		return s.set(c, v)
	}
	switch s.Kind {
	case "dur":
		d, err := time.ParseDuration(v)
		if err != nil {
			return "", fmt.Errorf("%s: «%s» no es una duración (usa 30s, 15m, 2h…)", s.Label, v)
		}
		if d.Seconds() < s.Min {
			return "", fmt.Errorf("%s: el mínimo es %s", s.Label, fmtDur(time.Duration(s.Min)*time.Second))
		}
		*s.ptrD(c) = d
		return fmtDur(d), nil
	case "int":
		n, err := strconv.Atoi(v)
		if err != nil || float64(n) < s.Min {
			return "", fmt.Errorf("%s: tiene que ser un número entero ≥ %v", s.Label, s.Min)
		}
		*s.ptrI(c) = n
		return strconv.Itoa(n), nil
	}
	if v == "" {
		return "", fmt.Errorf("%s: no puede quedar vacío", s.Label)
	}
	if s.Kind == "choice" && !slices.Contains(s.Options, v) {
		return "", fmt.Errorf("%s: opción desconocida «%s»", s.Label, v)
	}
	*s.ptrS(c) = v
	return v, nil
}

// fmtDur escribe 2h en vez de 2h0m0s.
func fmtDur(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = s[:len(s)-2]
	}
	if strings.HasSuffix(s, "h0m") {
		s = s[:len(s)-2]
	}
	return s
}

// setDownloadPath: la ruta de descarga es <biblioteca>/<carpeta>. Se parte en las dos piezas que usa el resto del
// código: la biblioteca (que se escanea para adoptar series) y la carpeta donde caen las nuevas.
func setDownloadPath(c *Config, v string) (string, error) {
	p := filepath.Clean(v)
	if v == "" || !filepath.IsAbs(p) {
		return "", fmt.Errorf("Carpeta de descarga: tiene que ser una ruta completa, empezando por /")
	}
	dir, base := filepath.Dir(p), filepath.Base(p)
	if dir == "/" {
		return "", fmt.Errorf("Carpeta de descarga: usa al menos dos niveles (p. ej. /mangas/Manga), no una carpeta de la raíz")
	}
	if !onVolume(p) {
		return "", fmt.Errorf("Carpeta de descarga: %s no está en un volumen montado. Dentro de Docker se escribiría en el propio contenedor y se perdería al recrearlo: monta la carpeta con -v (p. ej. -v %s:%s)", p, dir, dir)
	}
	if err := os.MkdirAll(p, 0o755); err != nil {
		return "", fmt.Errorf("Carpeta de descarga: no se puede crear %s (%v). En Docker solo existen las rutas montadas como volumen", p, err)
	}
	f, err := os.CreateTemp(p, ".rensaigo-escritura-*")
	if err != nil {
		return "", fmt.Errorf("Carpeta de descarga: no se puede escribir en %s (%v). Revisa los permisos o que el volumen no esté montado como solo lectura", p, err)
	}
	f.Close()
	os.Remove(f.Name())
	c.LibraryDir, c.DefaultCat = dir, base
	return p, nil
}

// onVolume: en un contenedor, la ruta tiene que estar en un volumen (otro dispositivo que la raíz «/» del contenedor).
// Fuera de Docker siempre vale.
func onVolume(p string) bool {
	if _, err := os.Stat("/.dockerenv"); err != nil {
		return true
	}
	for q := p; ; q = filepath.Dir(q) { // primer antepasado que exista
		var st, root syscall.Stat_t
		if syscall.Stat(q, &st) == nil {
			return syscall.Stat("/", &root) == nil && st.Dev != root.Dev
		}
		if q == "/" {
			return false
		}
	}
}
