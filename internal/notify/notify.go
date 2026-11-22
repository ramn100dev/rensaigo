// Package notify manda avisos fuera de la app: Telegram, ntfy, Discord, Gotify o un webhook propio.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"rensaigo/internal/i18n"
	"strings"
	"time"
)

// Channel: un destino. Los campos que use dependen del tipo.
type Channel struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`             // telegram | ntfy | discord | gotify | webhook
	URL    string `json:"url,omitempty"`    // ntfy: servidor+tema · discord/webhook: URL · gotify: servidor
	Token  string `json:"token,omitempty"`  // telegram: token del bot · gotify: token de la app · ntfy: token de acceso (opcional)
	Chat   string `json:"chat,omitempty"`   // telegram: chat id
	Events string `json:"events,omitempty"` // "chapters,alerts" (vacío = todo)
}

// Kinds: tipos admitidos, para el desplegable.
var Kinds = []struct{ Key, Name string }{
	{"telegram", "Telegram"}, {"ntfy", "ntfy"}, {"discord", "Discord"}, {"gotify", "Gotify"}, {"webhook", "Webhook (JSON)"},
}

// Msg: lo que se manda. Level: info | warn | bad (prioridad en ntfy/Gotify).
type Msg struct {
	Title string `json:"title"`
	Text  string `json:"message"`
	Link  string `json:"url,omitempty"`
	Kind  string `json:"kind"` // chapters | alerts | test
	Level string `json:"level"`
}

func (c Channel) Wants(kind string) bool {
	return kind == "test" || c.Events == "" || strings.Contains(","+c.Events+",", ","+kind+",")
}

// Validate comprueba que estén los campos que necesita cada tipo.
func (c Channel) Validate() error {
	need := func(v, what string) error {
		if strings.TrimSpace(v) == "" {
			return i18n.Errorf("falta %s", what)
		}
		return nil
	}
	httpURL := func(v string) error {
		u, err := url.Parse(v)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return i18n.Errorf("la dirección debe empezar por http:// o https://")
		}
		return nil
	}
	switch c.Kind {
	case "telegram":
		if err := need(c.Token, i18n.T("el token del bot")); err != nil {
			return err
		}
		return need(c.Chat, i18n.T("el chat id"))
	case "ntfy":
		if err := httpURL(c.URL); err != nil {
			return err
		}
		if u, _ := url.Parse(c.URL); strings.Trim(u.Path, "/") == "" {
			return i18n.Errorf("falta el tema al final de la dirección (https://ntfy.sh/mi-tema)")
		}
	case "gotify":
		if err := httpURL(c.URL); err != nil {
			return err
		}
		return need(c.Token, i18n.T("el token de la aplicación"))
	case "discord", "webhook":
		return httpURL(c.URL)
	default:
		return fmt.Errorf("tipo desconocido: %s", c.Kind)
	}
	return nil
}

// Label: cómo se muestra en la lista (sin secretos).
func (c Channel) Label() string {
	switch c.Kind {
	case "telegram":
		return "chat " + c.Chat
	case "ntfy", "gotify":
		return strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(c.URL, "https://"), "http://"), "/")
	case "discord":
		return "webhook de Discord"
	}
	if u, err := url.Parse(c.URL); err == nil {
		return u.Host
	}
	return c.URL
}

var client = &http.Client{Timeout: 20 * time.Second}

// Send manda un mensaje a un canal.
func Send(ctx context.Context, c Channel, m Msg) error {
	var req *http.Request
	var err error
	jsonReq := func(u string, v any) (*http.Request, error) {
		b, _ := json.Marshal(v)
		r, err := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(b))
		if err == nil {
			r.Header.Set("Content-Type", "application/json")
		}
		return r, err
	}
	text := m.Text
	switch c.Kind {
	case "telegram":
		body := "<b>" + htmlEsc(m.Title) + "</b>\n" + htmlEsc(m.Text)
		if m.Link != "" {
			body += "\n<a href=\"" + htmlEsc(m.Link) + "\">Abrir</a>"
		}
		req, err = jsonReq("https://api.telegram.org/bot"+strings.TrimSpace(c.Token)+"/sendMessage", map[string]any{
			"chat_id": strings.TrimSpace(c.Chat), "text": body, "parse_mode": "HTML", "disable_web_page_preview": true})
	case "ntfy":
		req, err = http.NewRequestWithContext(ctx, "POST", c.URL, strings.NewReader(text))
		if err == nil {
			req.Header.Set("Title", headerSafe(m.Title))
			req.Header.Set("Tags", map[string]string{"bad": "rotating_light", "warn": "warning", "info": "books"}[m.Level])
			req.Header.Set("Priority", map[string]string{"bad": "4", "warn": "3", "info": "3"}[m.Level])
			if m.Link != "" {
				req.Header.Set("Click", m.Link)
			}
			if c.Token != "" {
				req.Header.Set("Authorization", "Bearer "+c.Token)
			}
		}
	case "discord":
		content := "**" + m.Title + "**\n" + text
		if m.Link != "" {
			content += "\n<" + m.Link + ">"
		}
		req, err = jsonReq(c.URL, map[string]any{"content": trunc(content, 1900), "username": "rensaigo"})
	case "gotify":
		msg := text
		if m.Link != "" {
			msg += "\n" + m.Link
		}
		req, err = jsonReq(strings.TrimSuffix(c.URL, "/")+"/message?token="+url.QueryEscape(c.Token), map[string]any{
			"title": m.Title, "message": msg, "priority": map[string]int{"bad": 8, "warn": 5, "info": 4}[m.Level]})
	case "webhook":
		req, err = jsonReq(c.URL, m)
	default:
		return fmt.Errorf("tipo desconocido: %s", c.Kind)
	}
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "rensaigo")
	resp, err := client.Do(req)
	if err != nil {
		return scrub(err, c.Token)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("respuesta %d: %s", resp.StatusCode, scrub(fmt.Errorf("%s", strings.TrimSpace(string(b))), c.Token))
	}
	return nil
}

// scrub quita el token de los errores (el de Telegram va en la URL y net/http la incluye en el mensaje).
func scrub(err error, token string) error {
	if token == "" {
		return err
	}
	return fmt.Errorf("%s", strings.ReplaceAll(err.Error(), token, "…"))
}

func htmlEsc(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}

// headerSafe: las cabeceras HTTP no admiten saltos de línea; ntfy acepta UTF-8 en Title.
func headerSafe(s string) string { return strings.NewReplacer("\r", " ", "\n", " ").Replace(s) }

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
