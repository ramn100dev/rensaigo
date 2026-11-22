package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"rensaigo/internal/config"
	"rensaigo/internal/hostproc"
	"rensaigo/internal/i18n"
	"rensaigo/internal/jobs"
	"rensaigo/internal/jsengine"
)

// Sistema: ajustes editables, estado del proceso, acciones de mantenimiento y la cabecera (descargas y actividad).

func (s *Server) queue(w http.ResponseWriter, r *http.Request) {
	snap := s.Eng.Snapshot()
	s.render(w, "queue", M{"Snap": snap, "Host": s.Host.Stats()})
}

type Status struct {
	UptimeS    int64          `json:"uptime_s"`
	CoreRSSKb  int64          `json:"core_rss_kb"`
	CoreHWMKb  int64          `json:"core_hwm_kb"`
	GoHeapKb   uint64         `json:"go_heap_kb"`
	GoSysKb    uint64         `json:"go_sys_kb"`
	Goroutines int            `json:"goroutines"`
	Host       hostproc.Stats `json:"host"`
	Queue      jobs.Snapshot  `json:"queue"`
	ThumbBytes int64          `json:"thumb_cache_bytes"`
	ThumbFiles int            `json:"thumb_cache_files"`
	DBBytes    int64          `json:"db_bytes"`
	Series     int            `json:"series"`
	Chapters   int            `json:"chapters"`
}

func selfMem() (rss, hwm int64) {
	b, _ := os.ReadFile("/proc/self/status")
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) >= 2 {
			switch f[0] {
			case "VmRSS:":
				rss, _ = strconv.ParseInt(f[1], 10, 64)
			case "VmHWM:":
				hwm, _ = strconv.ParseInt(f[1], 10, 64)
			}
		}
	}
	return
}

func (s *Server) status() Status {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	st := Status{UptimeS: int64(time.Since(s.start).Seconds()), GoHeapKb: ms.HeapAlloc / 1024, GoSysKb: ms.Sys / 1024,
		Goroutines: runtime.NumGoroutine(), Host: s.Host.Stats(), Queue: s.Eng.Snapshot()}
	st.CoreRSSKb, st.CoreHWMKb = selfMem()
	st.ThumbBytes, st.ThumbFiles = s.Thumbs.Size()
	for _, suf := range []string{"", "-wal"} {
		if fi, err := os.Stat(s.Cfg.Path("rensaigo.db") + suf); err == nil {
			st.DBBytes += fi.Size()
		}
	}
	s.St.DB.QueryRow(`SELECT count(*) FROM series`).Scan(&st.Series)
	s.St.DB.QueryRow(`SELECT count(*) FROM chapter`).Scan(&st.Chapters)
	return st
}

func (s *Server) apiStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.status())
}

func (s *Server) system(w http.ResponseWriter, r *http.Request) {
	backups, _ := filepath.Glob(s.Cfg.Path("backup", "*.db"))
	var bk []M
	for _, b := range backups {
		if fi, err := os.Stat(b); err == nil {
			bk = append(bk, M{"name": filepath.Base(b), "size": fi.Size(), "at": fi.ModTime().Unix()})
		}
	}
	var last int64
	for _, b := range bk {
		if t := b["at"].(int64); t > last {
			last = t
		}
	}
	s.page(w, "system", "system", M{"St": s.status(), "Groups": s.settingGroups(), "Conns": s.connectedCount(),
		"Backups": bk, "LastBackup": last, "Cfg": s.Cfg, "Flare": s.Cfg.FlareSolverr != "", "Import": s.importData(),
		"JS": s.js().Installed(), "JSBytes": s.js().Size(),
		"JSNeed": s.rows(`SELECT name FROM source WHERE status='javascript' ORDER BY name`)})
}

func (s *Server) js() *jsengine.Engine { return &jsengine.Engine{Dir: s.Cfg.Path("jsengine")} }

func (s *Server) systemAction(w http.ResponseWriter, r *http.Request) {
	switch r.PathValue("action") {
	case "backup":
		if _, err := s.St.Backup(s.Cfg.Path("backup"), s.Cfg.BackupKeep); err != nil {
			fail(w, 500, err)
			return
		}
	case "host-stop":
		s.Host.Stop()
	case "js-install":
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
		defer cancel()
		if err := s.js().Install(ctx); err != nil {
			fail(w, 502, err)
			return
		}
		s.Host.Stop() // el siguiente arranque ya lleva el motor
		s.St.Event("javascript", "motor JavaScript (GraalJS) instalado y verificado: %.0f MB", float64(s.js().Size())/1e6)
	case "js-remove":
		s.js().Remove()
		s.Host.Stop()
	case "check-all":
		s.Eng.CheckAll()
	case "reset-peak":
		s.Host.ResetPeak()
	default:
		http.NotFound(w, r)
		return
	}
	w.Header().Set("HX-Redirect", "/system")
}

func (s *Server) hdrPill(w http.ResponseWriter, r *http.Request) {
	snap := s.Eng.Snapshot()
	seen, _ := strconv.ParseInt(s.St.Get("events_seen", "0"), 10, 64)
	var unread int
	s.St.DB.QueryRow(`SELECT count(*) FROM event WHERE id>?`, seen).Scan(&unread)
	al := s.alerts()
	lv := ""
	if len(al) > 0 {
		lv = al[0].Level // van ordenados: rojos primero
	}
	s.render(w, "hdr_pill", M{"Active": len(snap.Active), "Queued": snap.Counts["queued"], "Failed": snap.Counts["failed"], "QSeries": snap.QueuedSeries, "Paused": s.Eng.Paused(),
		"Unread": unread + len(al), "Level": lv})
}

func (s *Server) hdrQueue(w http.ResponseWriter, r *http.Request) {
	s.render(w, "hdr_queue", M{"Snap": s.Eng.Snapshot(), "Host": s.Host.Stats(), "Paused": s.Eng.Paused()})
}

// downloadsPause pausa o reanuda todas las descargas (lo que ya está bajando termina).
func (s *Server) downloadsPause(w http.ResponseWriter, r *http.Request) {
	p := !s.Eng.Paused()
	s.Eng.SetPaused(p)
	if p {
		s.St.Event("descarga", "descargas en pausa")
		setToast(w, i18n.T("Descargas en pausa"))
	} else {
		s.St.Event("descarga", "descargas reanudadas")
		setToast(w, i18n.T("Descargas reanudadas"))
	}
	w.Header().Set("HX-Trigger-After-Settle", `{"hdrchanged":true}`)
	s.hdrQueue(w, r)
}

func (s *Server) hdrEvents(w http.ResponseWriter, r *http.Request) {
	ev := s.rows(`SELECT id, at, kind, msg FROM event ORDER BY id DESC LIMIT 40`)
	seen, _ := strconv.ParseInt(s.St.Get("events_seen", "0"), 10, 64)
	for _, e := range ev {
		e["new"] = e["id"].(int64) > seen
		e["kind"] = i18n.T(e["kind"].(string))
	}
	if len(ev) > 0 {
		s.St.Set("events_seen", fmt.Sprint(ev[0]["id"]))
	}
	s.render(w, "hdr_events", M{"Events": ev, "Alerts": s.alerts()})
}

type settingGroup struct {
	Name  string
	Items []M
}

func (s *Server) settingGroups() []settingGroup {
	var out []settingGroup
	for i := range config.Settings {
		set := &config.Settings[i]
		if len(out) == 0 || out[len(out)-1].Name != i18n.T(set.Group) {
			out = append(out, settingGroup{Name: i18n.T(set.Group)})
		}
		_, custom := s.customSetting(set.Key)
		g := &out[len(out)-1]
		g.Items = append(g.Items, M{"Key": set.Key, "Label": i18n.T(set.Label), "Help": i18n.T(set.Help), "Kind": set.Kind, "Options": s.settingOptions(set), "Value": set.Value(s.Cfg), "Custom": custom})
	}
	return out
}

func (s *Server) customSetting(key string) (string, bool) {
	v := s.St.Get("cfg."+key, "")
	return v, v != ""
}

func (s *Server) settingsSave(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	var errs []string
	changed := 0
	for i := range config.Settings {
		set := &config.Settings[i]
		v, ok := r.Form[set.Key]
		if !ok || strings.TrimSpace(v[0]) == set.Value(s.Cfg) {
			continue
		}
		norm, err := set.Apply(s.Cfg, v[0])
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		s.St.Set("cfg."+set.Key, norm)
		changed++
	}
	s.Thumbs.MaxBytes = int64(s.Cfg.ThumbCacheMB) << 20
	if changed > 0 {
		if prev := i18n.Lang(); prev != s.Cfg.UILang && (prev == "es") != (s.Cfg.UILang == "es") {
			i18n.Set(s.Cfg.UILang)
			w.Header().Set("HX-Refresh", "true") // otro idioma: se recarga la página entera
		}
		s.ApplyAdult()
		s.Eng.Wake()
		log.Printf("ajustes: %d cambiados desde la web", changed)
	}
	msg := ""
	switch {
	case len(errs) > 0:
	case changed > 0:
		msg = i18n.Tf("%d ajustes guardados", changed)
	default:
		msg = i18n.T("Sin cambios")
	}
	s.render(w, "settings_form", M{"Groups": s.settingGroups(), "Errors": errs, "Msg": msg})
}

// settingOptions: valores del desplegable de un ajuste (las carpetas se leen de la biblioteca en cada visita).
func (s *Server) settingOptions(set *config.Setting) []string {
	switch set.Kind {
	case "choice":
		return set.Options
	}
	return nil
}
