// rensaigo: núcleo. Siempre encendido; el host JVM de extensiones se arranca bajo demanda.
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"rensaigo/internal/api"
	"rensaigo/internal/config"
	"rensaigo/internal/extrepo"
	"rensaigo/internal/hostproc"
	"rensaigo/internal/i18n"
	"rensaigo/internal/jobs"
	"rensaigo/internal/jsengine"
	"rensaigo/internal/library"
	"rensaigo/internal/logx"
	"rensaigo/internal/store"
)

// hostCommand compone la línea de la JVM. Flags medidos (heap mínimo con el que arrancan las extensiones):
// SerialGC + C1 + pilas de 512 k + heap pequeño; caché AOT de JDK 25 opcional en /config.
func hostCommand(cfg *config.Config, boost bool) []string {
	if cfg.HostCmd != "" {
		return strings.Fields(cfg.HostCmd)
	}
	// Directorio temporal propio de la JVM, vaciado en cada arranque: la caché HTTP de OkHttp (5 MB) y las
	// librerías nativas extraídas se crean ahí y, si no, se acumularían arranque tras arranque.
	jtmp := cfg.Path("cache", "jvm-tmp")
	os.RemoveAll(jtmp)
	os.MkdirAll(jtmp, 0o755)
	xmx := cfg.HostXmx
	if boost {
		xmx = cfg.HostXmxBoost
	}
	args := []string{cfg.HostJava,
		"-Xmx" + xmx, "-Xms8m", "-Xss512k", "-XX:+UseSerialGC", "-XX:TieredStopAtLevel=1",
		"-XX:MaxMetaspaceSize=96m", "-XX:ReservedCodeCacheSize=" + cfg.HostCodeCache, "-XX:MinHeapFreeRatio=10", "-XX:MaxHeapFreeRatio=25",
		"-XX:+ExitOnOutOfMemoryError",
		"-Djava.io.tmpdir=" + jtmp,
		"-Dsuwayomi.tachidesk.config.server.rootDir=" + cfg.Path("host"),
	}
	if cfg.HostAOT {
		aot := cfg.Path("cache", "host.aot")
		if _, err := os.Stat(aot); err == nil {
			args = append(args, "-XX:AOTCache="+aot, "-Xlog:aot=off", "-Xlog:cds=off")
		} else {
			// primer uso: el host graba la caché al apagarse limpiamente; los siguientes arranques la aprovechan
			args = append(args, "-XX:AOTCacheOutput="+aot, "-Xlog:aot=off", "-Xlog:cds=off")
		}
	}
	// Capa opcional de JavaScript: si está instalada, sus jars van detrás del host en el classpath.
	js := &jsengine.Engine{Dir: cfg.Path("jsengine")}
	if cp := js.Classpath(); cp != "" {
		return append(args, "--enable-native-access=ALL-UNNAMED", "-cp", cfg.HostJar+":"+cp, "rensaigo.host.MainKt")
	}
	return append(args, "-jar", cfg.HostJar)
}

func main() {
	cfg := config.Load()
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(int64(cfg.MemLimitMiB) << 20)
	}
	for _, d := range []string{"", "logs", "extensions", "cache", "tmp", "host", "backup"} {
		if err := os.MkdirAll(cfg.Path(d), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "no se puede crear", cfg.Path(d), err)
			os.Exit(1)
		}
	}

	max := int64(cfg.LogMaxMB) << 20
	appLog, err := logx.NewRotating(cfg.Path("logs", "rensaigo.log"), max)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	hostLog, _ := logx.NewRotating(cfg.Path("logs", "host.log"), max)
	log.SetOutput(io.MultiWriter(os.Stderr, appLog))
	log.SetFlags(log.LstdFlags)

	migrateOldName(cfg)
	st, err := store.Open(cfg.Path("rensaigo.db"))
	if err != nil {
		log.Fatalf("base de datos: %v", err)
	}
	defer st.Close()
	if err := st.EnableSecrets(cfg.Path("secret.key")); err != nil {
		log.Fatalf("clave de cifrado: %v", err)
	}
	// ajustes guardados desde la web: mandan sobre las variables de entorno
	for i := range config.Settings {
		set := &config.Settings[i]
		if v := st.Get("cfg."+set.Key, ""); v != "" {
			if _, err := set.Apply(cfg, v); err != nil {
				log.Printf("ajuste guardado ignorado: %v", err)
			}
		}
	}
	i18n.Set(cfg.UILang)

	// glibc crea hasta 8 arenas de malloc por núcleo; con los hilos de OkHttp eso infla el RSS del host. Dos bastan.
	env := []string{}
	if os.Getenv("MALLOC_ARENA_MAX") == "" {
		env = append(env, "MALLOC_ARENA_MAX=2")
	}
	if cfg.FlareSolverr != "" {
		env = append(env, "RENSAIGO_FLARESOLVERR_URL="+cfg.FlareSolverr)
	}
	host := hostproc.New(hostproc.Options{
		Argv: hostCommand(cfg, false), Env: env, KeepAlive: cfg.HostKeepAlive, Stderr: hostLog,
		IdleGC: cfg.HostIdleGC, RecycleKb: int64(cfg.HostRecycleMB) << 10,
		BoostFor: 24 * time.Hour,
		OnOOM: func() {
			st.Event("host", "el host se quedó sin memoria con %s de heap; se repite el trabajo con %s (durante 24 h)", cfg.HostXmx, cfg.HostXmxBoost)
		},
		OnExit: func(clean bool) {
			if !clean {
				st.Event("host", "el host de extensiones se cayó; se reiniciará con la siguiente orden")
			}
		},
	})
	// la línea de la JVM depende de si ya existe la caché AOT: se recalcula en cada arranque del host
	host.SetCommand(func(boost bool) []string { return hostCommand(cfg, boost) })
	defer host.Close()

	repo := extrepo.New(st, host, cfg.RepoURL, cfg.RepoFingerprint, cfg.Path("extensions"))
	eng := jobs.New(cfg, st, host, repo)
	eng.Recover()
	thumbs := &library.Thumbs{Dir: cfg.Path("cache", "thumbs"), MaxBytes: int64(cfg.ThumbCacheMB) << 20, Width: 300}

	srv, err := api.New(cfg, st, host, repo, eng, thumbs)
	if err != nil {
		log.Fatalf("plantillas: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go srv.QuickScan(true) // una instalación nueva sobre una biblioteca existente no aparece vacía
	go eng.Run(ctx)
	go srv.TrackLoop(ctx)
	eng.OnChange = srv.Touch
	eng.OnDownloaded = srv.OnDownloaded
	go srv.NotifyLoop(ctx)
	go srv.SidecarLoop(ctx)
	go srv.CoverageLoop(ctx)
	go backups(ctx, cfg, st)
	go extUpdates(ctx, cfg, st, repo)

	hs := &http.Server{Addr: cfg.Addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Printf("rensaigo escuchando en %s (config=%s, biblioteca=%s, auth=%v, keep-alive host=%s, comprobación cada %s)",
			cfg.Addr, cfg.ConfigDir, cfg.LibraryDir, cfg.Password != "", cfg.HostKeepAlive, cfg.CheckInterval)
		if err := hs.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Printf("apagando…")
	cancel()
	sctx, c2 := context.WithTimeout(context.Background(), 5*time.Second)
	hs.Shutdown(sctx)
	c2()
	host.Close()
}

// backups mantiene UNA copia compacta reciente de la BD (más las que diga la retención). Nada de carpetas que crecen.
func backups(ctx context.Context, cfg *config.Config, st *store.Store) {
	dir := cfg.Path("backup")
	for {
		newest := time.Time{}
		files, _ := filepath.Glob(filepath.Join(dir, "rensaigo-*.db"))
		for _, f := range files {
			if fi, err := os.Stat(f); err == nil && fi.ModTime().After(newest) {
				newest = fi.ModTime()
			}
		}
		if time.Since(newest) >= cfg.BackupInterval {
			if name, err := st.Backup(dir, cfg.BackupKeep); err != nil {
				log.Printf("backup: %v", err)
			} else {
				log.Printf("backup: %s", filepath.Base(name))
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Minute):
		}
	}
}

// extUpdates mantiene al día el índice y las extensiones instaladas (las webs cambian y Keiyoushi publica arreglos).
func extUpdates(ctx context.Context, cfg *config.Config, st *store.Store, repo *extrepo.Repo) {
	if cfg.ExtUpdate <= 0 {
		return
	}
	for {
		last, _ := strconv.ParseInt(st.Get("repo_refreshed", "0"), 10, 64)
		wait := time.Until(time.Unix(last, 0).Add(cfg.ExtUpdate))
		if last == 0 { // sin índice todavía: no se descarga nada hasta que el usuario lo pida
			wait = cfg.ExtUpdate
		}
		if wait < time.Minute {
			wait = time.Minute
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		uctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
		n, err := repo.UpdateInstalled(uctx)
		cancel()
		if err != nil {
			log.Printf("actualización de extensiones: %v", err)
		}
		if n > 0 {
			st.Event("extensión", "actualización automática: %d extensiones actualizadas", n)
		}
	}
}

// migrateOldName: antes se llamaba rensaio-lite; su base de datos (y su log) pasan al nombre nuevo la primera vez.
func migrateOldName(cfg *config.Config) {
	if _, err := os.Stat(cfg.Path("rensaigo.db")); err == nil {
		return
	}
	if _, err := os.Stat(cfg.Path("rensaio-lite.db")); err != nil {
		return
	}
	for _, suf := range []string{"", "-wal", "-shm"} {
		if err := os.Rename(cfg.Path("rensaio-lite.db"+suf), cfg.Path("rensaigo.db"+suf)); err != nil && !os.IsNotExist(err) {
			log.Fatalf("no se pudo renombrar rensaio-lite.db%s: %v", suf, err)
		}
	}
	log.Printf("base de datos de rensaio-lite pasada a rensaigo.db")
}
