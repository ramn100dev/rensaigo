// Package config lee la configuración del entorno (variables RENSAIGO_*).
package config

import (
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Version la pone el build (-ldflags "-X rensaigo/internal/config.Version=…"); «dev» si se compila a mano.
var Version = "dev"

type Config struct {
	Addr        string
	ConfigDir   string // BD, jars, cookies del host, cachés, logs, backup
	LibraryDir  string // <Categoría>/<Serie>/*.cbz
	Password    string // vacío = sin autenticación
	MemLimitMiB int    // GOMEMLIMIT por defecto si no viene del entorno

	RepoURL         string // repo.json de Keiyoushi
	RepoFingerprint string // huella SHA-256 del certificado que firma los jars (se fija; no se confía en lo que diga la red)

	HostCmd       string // comando completo del host; vacío = se compone con HostJava/HostJar/HostXmx
	HostJava      string
	HostJar       string
	HostXmx       string
	HostXmxBoost  string // heap con el que se reintenta tras un OutOfMemoryError del host
	HostKeepAlive time.Duration
	HostIdleGC    bool // GC completo del host al quedar inactivo
	HostRecycleMB int  // apagar el host en reposo si supera este RSS tras el GC (0 = nunca)
	HostCodeCache string
	HostAOT       bool // opcional: caché AOT de JDK 25 en ConfigDir. Apagado por defecto: el arranque de entrenamiento gasta más RAM
	FlareSolverr  string

	CheckInterval   time.Duration
	DLConcurrency   int           // capítulos a la vez en total
	PerSource       int           // capítulos a la vez por source
	PageConcurrency int           // imágenes a la vez dentro de un capítulo
	RateInterval    time.Duration // separación mínima entre peticiones a un mismo source
	MaxAttempts     int

	ThumbCacheMB   int
	LogMaxMB       int
	BackupInterval time.Duration
	BackupKeep     int
	DefaultCat     string
	PrefLang       string
	UILang         string        // idioma de la interfaz: "en" o "es"
	Adult          string        // "sí" permite contenido +18 (extensiones +18 y versiones +18 de las normales)
	ExtUpdate      time.Duration // cada cuánto se actualiza el índice y las extensiones instaladas (0 = nunca)
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
func envInt(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return v
	}
	return def
}
func envDur(k string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(os.Getenv(k)); err == nil {
		return v
	}
	return def
}

func Load() *Config {
	c := &Config{
		Addr:            env("RENSAIGO_ADDR", ":9840"),
		ConfigDir:       env("RENSAIGO_CONFIG", "/config"),
		LibraryDir:      env("RENSAIGO_LIBRARY", "/library"),
		Password:        os.Getenv("RENSAIGO_PASSWORD"),
		MemLimitMiB:     envInt("RENSAIGO_MEMLIMIT_MIB", 24),
		RepoURL:         env("RENSAIGO_REPO_URL", "https://raw.githubusercontent.com/keiyoushi/extensions/repo/repo.json"),
		RepoFingerprint: env("RENSAIGO_REPO_FINGERPRINT", "9add655a78e96c4ec7a53ef89dccb557cb5d767489fac5e785d671a5a75d4da2"),
		HostCmd:         os.Getenv("RENSAIGO_HOST_CMD"),
		HostJava:        env("RENSAIGO_HOST_JAVA", "/opt/jre/bin/java"),
		HostJar:         env("RENSAIGO_HOST_JAR", "/opt/host/lib/rensaigo-host-0.1.jar"),
		HostXmx:         env("RENSAIGO_HOST_XMX", "28m"),
		HostXmxBoost:    env("RENSAIGO_HOST_XMX_BOOST", "96m"),
		HostKeepAlive:   envDur("RENSAIGO_HOST_KEEPALIVE", 90*time.Second),
		HostIdleGC:      env("RENSAIGO_HOST_IDLE_GC", "1") == "1",
		HostRecycleMB:   envInt("RENSAIGO_HOST_RECYCLE_MB", 140),
		HostCodeCache:   env("RENSAIGO_HOST_CODECACHE", "24m"),
		HostAOT:         env("RENSAIGO_HOST_AOT", "0") == "1",
		FlareSolverr:    os.Getenv("RENSAIGO_FLARESOLVERR_URL"),
		CheckInterval:   envDur("RENSAIGO_CHECK_INTERVAL", 2*time.Hour),
		Adult:           env("RENSAIGO_ADULT", "no"),
		DLConcurrency:   envInt("RENSAIGO_DL_CONCURRENCY", 3),
		PerSource:       envInt("RENSAIGO_DL_PER_SOURCE", 1),
		PageConcurrency: envInt("RENSAIGO_PAGE_CONCURRENCY", 2),
		RateInterval:    envDur("RENSAIGO_RATE_INTERVAL", 200*time.Millisecond),
		MaxAttempts:     envInt("RENSAIGO_MAX_ATTEMPTS", 5),
		ThumbCacheMB:    envInt("RENSAIGO_THUMB_CACHE_MB", 64),
		LogMaxMB:        envInt("RENSAIGO_LOG_MAX_MB", 5),
		BackupInterval:  envDur("RENSAIGO_BACKUP_INTERVAL", 24*time.Hour),
		BackupKeep:      envInt("RENSAIGO_BACKUP_KEEP", 2),
		ExtUpdate:       envDur("RENSAIGO_EXT_UPDATE_INTERVAL", 24*time.Hour),
		DefaultCat:      env("RENSAIGO_DEFAULT_CATEGORY", "Manga"),
		PrefLang:        env("RENSAIGO_LANG", "en"),
		UILang:          env("RENSAIGO_UI_LANG", "en"),
	}
	return c
}

func (c *Config) Path(parts ...string) string {
	return filepath.Join(append([]string{c.ConfigDir}, parts...)...)
}
