package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"rensaigo/internal/i18n"
	"rensaigo/internal/library"
	"rensaigo/internal/notify"
)

// Notificaciones fuera de la app (Telegram, ntfy, Discord, Gotify, webhook). Dos tipos de aviso:
//   - capítulos nuevos: se juntan y salen como mucho cada 2 minutos (una serie que baja 40 de golpe = un mensaje)
//   - avisos de la campana (serie parada, source que falla, fallidos): cada uno una vez; vuelve a salir solo si
//     el problema cambia, igual que en la campana.

type newChap struct {
	title string
	nums  []float64
}

type notifier struct {
	mu      sync.Mutex
	pending map[int64]*newChap
}

func (s *Server) channels() []notify.Channel {
	var cs []notify.Channel
	json.Unmarshal([]byte(s.St.Get("notify.channels", "[]")), &cs)
	return cs
}

func (s *Server) saveChannels(cs []notify.Channel) {
	b, _ := json.Marshal(cs)
	s.St.Set("notify.channels", string(b))
}

func (s *Server) notifyBase() string { return strings.TrimSuffix(s.St.Get("notify.base", ""), "/") }

// OnDownloaded: para el motor (capítulo nuevo en disco).
func (s *Server) OnDownloaded(sid int64, title string, number float64) {
	s.ntf.mu.Lock()
	defer s.ntf.mu.Unlock()
	if s.ntf.pending == nil {
		s.ntf.pending = map[int64]*newChap{}
	}
	p := s.ntf.pending[sid]
	if p == nil {
		p = &newChap{title: title}
		s.ntf.pending[sid] = p
	}
	p.nums = append(p.nums, number)
}

// NotifyLoop manda lo pendiente cada 2 minutos y revisa los avisos cada 10.
func (s *Server) NotifyLoop(ctx context.Context) {
	t := time.NewTicker(2 * time.Minute)
	defer t.Stop()
	tick := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.flushChapters(ctx)
		if tick%5 == 0 {
			s.notifyAlerts(ctx)
		}
		tick++
	}
}

func (s *Server) broadcast(ctx context.Context, m notify.Msg) {
	for _, c := range s.channels() {
		if !c.Wants(m.Kind) {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		if err := notify.Send(cctx, c, m); err != nil {
			log.Printf("notificación por %s: %v", c.Kind, err)
			s.St.Event("notificación", "no se pudo avisar por %s (%s): %v", kindName(c.Kind), c.Label(), err)
		}
		cancel()
	}
}

func kindName(k string) string {
	for _, x := range notify.Kinds {
		if x.Key == k {
			return x.Name
		}
	}
	return k
}

func (s *Server) flushChapters(ctx context.Context) {
	s.ntf.mu.Lock()
	pend := s.ntf.pending
	s.ntf.pending = nil
	s.ntf.mu.Unlock()
	if len(pend) == 0 || len(s.channels()) == 0 {
		return
	}
	type item struct {
		sid int64
		*newChap
	}
	var items []item
	for sid, p := range pend {
		sort.Float64s(p.nums)
		items = append(items, item{sid, p})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].title < items[j].title })
	desc := func(nums []float64) string {
		if len(nums) == 1 {
			return i18n.Tf("capítulo %s", library.FormatNumber(nums[0]))
		}
		return i18n.Tf("%d capítulos (%s–%s)", len(nums), library.FormatNumber(nums[0]), library.FormatNumber(nums[len(nums)-1]))
	}
	base := s.notifyBase()
	if len(items) == 1 {
		it := items[0]
		m := notify.Msg{Kind: "chapters", Level: "info", Title: it.title, Text: i18n.Tf("Nuevo: %s", desc(it.nums))}
		if base != "" {
			m.Link = fmt.Sprintf("%s/library/%d", base, it.sid)
		}
		s.broadcast(ctx, m)
		return
	}
	var lines []string
	total := 0
	for _, it := range items {
		lines = append(lines, "• "+it.title+": "+desc(it.nums))
		total += len(it.nums)
	}
	m := notify.Msg{Kind: "chapters", Level: "info", Title: i18n.Tf("%d capítulos nuevos", total), Text: strings.Join(lines, "\n")}
	if base != "" {
		m.Link = base + "/library"
	}
	s.broadcast(ctx, m)
}

func (s *Server) notifyAlerts(ctx context.Context) {
	cs := s.channels()
	want := false
	for _, c := range cs {
		want = want || c.Wants("alerts")
	}
	if !want {
		return
	}
	sent := map[string]string{}
	json.Unmarshal([]byte(s.St.Get("notify.sent", "{}")), &sent)
	now := map[string]string{}
	base := s.notifyBase()
	for _, a := range s.alerts() {
		now[a.Key] = a.Sig + "|" + a.Level
		if sent[a.Key] == now[a.Key] {
			continue
		}
		m := notify.Msg{Kind: "alerts", Level: a.Level, Title: "rensaigo", Text: a.Text}
		if base != "" {
			m.Link = base + a.Href
		}
		s.broadcast(ctx, m)
	}
	b, _ := json.Marshal(now) // los resueltos salen de la lista: si vuelven, se avisa otra vez
	s.St.Set("notify.sent", string(b))
}

// ---------------------------------------------------------------- panel (en Conexiones)

func (s *Server) notifyPanel(w http.ResponseWriter, r *http.Request) {
	s.renderNotify(w, r, "")
}

func (s *Server) renderNotify(w http.ResponseWriter, r *http.Request, msg string) {
	type row struct {
		notify.Channel
		Name, Label string
		Chapters    bool
		Alerts      bool
	}
	var rows []row
	for _, c := range s.channels() {
		rows = append(rows, row{c, kindName(c.Kind), c.Label(), c.Wants("chapters"), c.Wants("alerts")})
	}
	base := s.notifyBase()
	if base == "" {
		base = guessBase(r)
	}
	s.render(w, "notify_panel", M{"Rows": rows, "Kinds": notify.Kinds, "Base": base, "Msg": msg})
}

func guessBase(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (s *Server) notifyAction(w http.ResponseWriter, r *http.Request) {
	cs := s.channels()
	id := r.FormValue("id")
	idx := -1
	for i, c := range cs {
		if c.ID == id {
			idx = i
		}
	}
	msg := ""
	switch r.PathValue("act") {
	case "add":
		k := r.FormValue("kind") // cada tipo tiene sus campos: <tipo>_url, <tipo>_token, <tipo>_chat
		c := notify.Channel{Kind: k, URL: strings.TrimSpace(r.FormValue(k + "_url")),
			Token: strings.TrimSpace(r.FormValue(k + "_token")), Chat: strings.TrimSpace(r.FormValue(k + "_chat")), Events: "chapters,alerts"}
		if err := c.Validate(); err != nil {
			fail(w, 400, err)
			return
		}
		b := make([]byte, 6)
		rand.Read(b)
		c.ID = hex.EncodeToString(b)
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		err := notify.Send(ctx, c, notify.Msg{Kind: "test", Level: "info", Title: "rensaigo", Text: i18n.T("Notificaciones conectadas ✔")})
		cancel()
		if err != nil {
			fail(w, 502, i18n.Errorf("no se pudo mandar el mensaje de prueba: %v", err))
			return
		}
		cs = append(cs, c)
		if s.notifyBase() == "" {
			s.St.Set("notify.base", guessBase(r))
		}
		msg = i18n.T("Añadido: te ha llegado un mensaje de prueba")
	case "del":
		if idx >= 0 {
			cs = append(cs[:idx], cs[idx+1:]...)
		}
	case "test":
		if idx >= 0 {
			ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
			err := notify.Send(ctx, cs[idx], notify.Msg{Kind: "test", Level: "info", Title: "rensaigo", Text: i18n.T("Mensaje de prueba")})
			cancel()
			if err != nil {
				fail(w, 502, err)
				return
			}
			setToast(w, i18n.T("Enviado"))
		}
	case "event": // activa o desactiva un tipo de aviso en un canal
		if idx >= 0 {
			ev := r.FormValue("ev")
			on := map[string]bool{"chapters": cs[idx].Wants("chapters"), "alerts": cs[idx].Wants("alerts")}
			on[ev] = !on[ev]
			var l []string
			for _, k := range []string{"chapters", "alerts"} {
				if on[k] {
					l = append(l, k)
				}
			}
			cs[idx].Events = strings.Join(l, ",")
			if cs[idx].Events == "" {
				cs[idx].Events = "none"
			}
		}
	case "base":
		b := strings.TrimSpace(r.FormValue("base"))
		if b != "" && !strings.HasPrefix(b, "http://") && !strings.HasPrefix(b, "https://") {
			fail(w, 400, i18n.Errorf("la dirección debe empezar por http:// o https://"))
			return
		}
		s.St.Set("notify.base", b)
		setToast(w, i18n.T("Guardado"))
	default:
		http.NotFound(w, r)
		return
	}
	s.saveChannels(cs)
	s.renderNotify(w, r, msg)
}
