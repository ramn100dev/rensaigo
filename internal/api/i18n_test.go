package api

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"rensaigo/internal/config"
	"rensaigo/internal/i18n"
	"rensaigo/web"
)

// Cada texto que se traduce (t/tf/tn en plantillas; i18n.T/Tf/Tn/Errorf y Event en Go) tiene su versión inglesa.
func TestI18nCatalog(t *testing.T) {
	missing := map[string]bool{}
	need := func(s string) {
		if !i18n.Has(s) {
			missing[s] = true
		}
	}
	str := `"((?:[^"\\]|\\.)*)"`
	tplRe := regexp.MustCompile(`(?:\{\{-?|\(|\|)\s*(?:t|tf)\s+` + str)
	tnRe := regexp.MustCompile(`(?:\{\{-?|\()\s*tn\s+(?:\([^)]*\)|\S+)\s+` + str + `\s+` + str)
	fs.WalkDir(web.FS, "templates", func(p string, d fs.DirEntry, err error) error {
		if d.IsDir() {
			return nil
		}
		b, _ := fs.ReadFile(web.FS, p)
		for _, m := range tplRe.FindAllStringSubmatch(string(b), -1) {
			need(unq(m[1]))
		}
		for _, m := range tnRe.FindAllStringSubmatch(string(b), -1) {
			need(unq(m[1]))
			need(unq(m[2]))
		}
		return nil
	})
	goRe := regexp.MustCompile(`i18n\.(?:T|Tf|Errorf)\(\s*` + str)
	goTn := regexp.MustCompile(`i18n\.Tn\([^,]+,\s*` + str + `,\s*` + str)
	evRe := regexp.MustCompile(`\.Event\(\s*([^,]+),\s*` + str)
	filepath.WalkDir("..", func(p string, d fs.DirEntry, err error) error {
		if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") || strings.Contains(p, "/i18n/") {
			return nil
		}
		b, _ := os.ReadFile(p)
		for _, m := range goRe.FindAllStringSubmatch(string(b), -1) {
			need(unq(m[1]))
		}
		for _, m := range goTn.FindAllStringSubmatch(string(b), -1) {
			need(unq(m[1]))
			need(unq(m[2]))
		}
		for _, m := range evRe.FindAllStringSubmatch(string(b), -1) {
			if k := strings.TrimSpace(m[1]); strings.HasPrefix(k, `"`) {
				need(unq(strings.Trim(k, `"`)))
			}
			need(unq(m[2]))
		}
		return nil
	})
	for _, s := range jsTexts {
		need(s)
	}
	for _, set := range config.Settings {
		need(set.Group)
		need(set.Label)
		need(set.Help)
	}
	for _, m := range []map[string]string{stateLabel, trackLabel} {
		for _, v := range m {
			need(v)
		}
	}
	for _, v := range pubLabel {
		need(v)
	}
	for s := range missing {
		t.Errorf("sin traducción: %q", s)
	}
}

func unq(s string) string {
	u, err := strconv.Unquote(`"` + s + `"`)
	if err != nil {
		return s
	}
	return u
}

// Ningún texto visible de las plantillas se queda fuera de t/tf/tn.
func TestI18nTemplatesNoLooseText(t *testing.T) {
	action := regexp.MustCompile(`\{\{[\s\S]*?\}\}`)
	blocks := regexp.MustCompile(`(?s)<(script|style|svg)[^>]*>.*?</(script|style|svg)>`)
	textRe := regexp.MustCompile(`>([^<>]+)<`)
	attrRe := regexp.MustCompile(`\b(title|placeholder|aria-label|data-tip|data-modal|hx-confirm|alt)="([^"]*)"`)
	word := regexp.MustCompile(`\p{L}{2,}`)
	ok := map[string]bool{}
	for _, w := range strings.Fields("rensaigo Sources Source JS px OPDS MB ms ext Cloudflare Client ID Secret Redirect URL Telegram ntfy Discord Gotify rensai go Chunky Panels KOReader v https http") {
		ok[w] = true
	}
	fs.WalkDir(web.FS, "templates", func(p string, d fs.DirEntry, err error) error {
		if d.IsDir() {
			return nil
		}
		b, _ := fs.ReadFile(web.FS, p)
		s := blocks.ReplaceAllString(string(b), "")
		s = action.ReplaceAllString(s, "")
		var chunks []string
		for _, m := range textRe.FindAllStringSubmatch(s, -1) {
			chunks = append(chunks, m[1])
		}
		for _, m := range attrRe.FindAllStringSubmatch(s, -1) {
			chunks = append(chunks, m[2])
		}
		for _, c := range chunks {
			for _, w := range word.FindAllString(c, -1) {
				if !ok[w] {
					t.Errorf("%s: texto sin traducir %q (en %q)", p, w, strings.TrimSpace(c))
				}
			}
		}
		return nil
	})
}
