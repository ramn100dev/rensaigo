// Package library: biblioteca en disco (CBZ + ComicInfo.xml), lector, miniaturas e importador.
package library

import (
	"archive/zip"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"rensaigo/internal/i18n"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ComicInfo: subconjunto del esquema que escribimos y leemos.
type ComicInfo struct {
	XMLName     xml.Name `xml:"ComicInfo"`
	Title       string   `xml:"Title"`
	Series      string   `xml:"Series"`
	Number      string   `xml:"Number"`
	Count       int      `xml:"Count,omitempty"`
	Summary     string   `xml:"Summary,omitempty"`
	Notes       string   `xml:"Notes,omitempty"`
	Year        int      `xml:"Year,omitempty"`
	Month       int      `xml:"Month,omitempty"`
	Day         int      `xml:"Day,omitempty"`
	Writer      string   `xml:"Writer,omitempty"`
	Penciller   string   `xml:"Penciller,omitempty"`
	Translator  string   `xml:"Translator,omitempty"`
	Publisher   string   `xml:"Publisher,omitempty"`
	Genre       string   `xml:"Genre,omitempty"`
	Tags        string   `xml:"Tags,omitempty"`
	Web         string   `xml:"Web,omitempty"`
	PageCount   int      `xml:"PageCount"`
	LanguageISO string   `xml:"LanguageISO,omitempty"`
	Format      string   `xml:"Format,omitempty"`
	Manga       string   `xml:"Manga,omitempty"`
}

var badChars = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]`)

// SafeName limpia un nombre para usarlo como carpeta o fichero.
func SafeName(s string) string {
	s = strings.TrimSpace(badChars.ReplaceAllString(s, " "))
	s = strings.Join(strings.Fields(s), " ")
	s = strings.TrimRight(s, ". ")
	if len(s) > 150 { // en bytes, sin partir un carácter de varios bytes (títulos en japonés, coreano…)
		cut := 150
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = strings.TrimRight(s[:cut], ". ")
	}
	if s == "" {
		s = "sin título"
	}
	return s
}

// FormatNumber: 1 -> "01", 31.5 -> "31.5", 120 -> "120".
func FormatNumber(n float64) string {
	if n < 0 {
		return "00"
	}
	s := strconv.FormatFloat(n, 'f', -1, 64)
	ip, frac, has := strings.Cut(s, ".")
	if len(ip) < 2 {
		ip = "0" + ip
	}
	if has {
		return ip + "." + frac
	}
	return ip
}

// ChapterFile compone el nombre con el mismo esquema que Rensaio:
// [Source][es] Serie (Capítulo N) NN.cbz
func ChapterFile(source, lang, series, chapterName string, number float64) string {
	name := strings.TrimSpace(chapterName)
	if name == "" {
		name = "Capítulo " + strings.TrimLeft(FormatNumber(number), "0")
	}
	// el número va aparte: si el nombre es muy largo se recorta lo de delante, nunca el número (si no, dos capítulos
	// acabarían con el mismo archivo)
	return SafeName(fmt.Sprintf("[%s][%s] %s (%s)", source, lang, series, name)) + " " + FormatNumber(number) + ".cbz"
}

func IsImage(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".webp", ".jpg", ".jpeg", ".png", ".gif", ".avif", ".jxl", ".bmp":
		return true
	}
	return false
}

// SniffExt deduce la extensión por los primeros bytes del fichero.
func SniffExt(head []byte) string {
	switch {
	case len(head) >= 3 && head[0] == 0xff && head[1] == 0xd8:
		return ".jpg"
	case len(head) >= 8 && string(head[1:4]) == "PNG":
		return ".png"
	case len(head) >= 12 && string(head[0:4]) == "RIFF" && string(head[8:12]) == "WEBP":
		return ".webp"
	case len(head) >= 4 && string(head[0:3]) == "GIF":
		return ".gif"
	case len(head) >= 12 && string(head[4:8]) == "ftyp" && strings.HasPrefix(string(head[8:12]), "avi"):
		return ".avif"
	}
	return ""
}

// WriteCBZ empaqueta las imágenes (ya en disco) en un CBZ SIN recomprimir (método store) y en streaming:
// nunca hay más de un búfer de copia en memoria. Escribe en <dest>.part, hace fsync y renombra, de modo que
// un CBZ a medias jamás aparece con su nombre definitivo.
func WriteCBZ(dest string, images []string, info *ComicInfo) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return 0, err
	}
	part := dest + ".part"
	f, err := os.Create(part)
	if err != nil {
		return 0, err
	}
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(part)
		}
	}()
	zw := zip.NewWriter(f)
	buf := make([]byte, 64<<10)
	width := len(strconv.Itoa(len(images)))
	if width < 3 {
		width = 3
	}
	for i, img := range images {
		in, err := os.Open(img)
		if err != nil {
			return 0, err
		}
		head := make([]byte, 16)
		n, _ := io.ReadFull(in, head)
		ext := SniffExt(head[:n])
		if ext == "" {
			in.Close()
			return 0, i18n.Errorf("la página %d no es una imagen reconocida", i+1)
		}
		if _, err := in.Seek(0, io.SeekStart); err != nil {
			in.Close()
			return 0, err
		}
		w, err := zw.CreateHeader(&zip.FileHeader{Name: fmt.Sprintf("%0*d%s", width, i+1, ext), Method: zip.Store})
		if err == nil {
			_, err = io.CopyBuffer(w, in, buf)
		}
		in.Close()
		if err != nil {
			return 0, err
		}
	}
	info.PageCount = len(images)
	w, err := zw.CreateHeader(&zip.FileHeader{Name: "ComicInfo.xml", Method: zip.Deflate})
	if err != nil {
		return 0, err
	}
	io.WriteString(w, xml.Header)
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(info); err != nil {
		return 0, err
	}
	if err := zw.Close(); err != nil {
		return 0, err
	}
	if err := f.Sync(); err != nil {
		return 0, err
	}
	st, _ := f.Stat()
	if err := f.Close(); err != nil {
		return 0, err
	}
	if err := os.Rename(part, dest); err != nil {
		return 0, err
	}
	ok = true
	if d, err := os.Open(filepath.Dir(dest)); err == nil {
		d.Sync()
		d.Close()
	}
	return st.Size(), nil
}

// CBZInfo abre un CBZ y devuelve su ComicInfo (si existe) y sus páginas ordenadas.
type CBZ struct {
	zr    *zip.ReadCloser
	Pages []*zip.File
	Info  *ComicInfo
}

func OpenCBZ(path string, withInfo bool) (*CBZ, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	c := &CBZ{zr: zr}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if withInfo && strings.EqualFold(f.Name, "ComicInfo.xml") {
			if rc, err := f.Open(); err == nil {
				var ci ComicInfo
				if xml.NewDecoder(io.LimitReader(rc, 1<<20)).Decode(&ci) == nil {
					c.Info = &ci
				}
				rc.Close()
			}
		} else if IsImage(f.Name) {
			c.Pages = append(c.Pages, f)
		}
	}
	sort.Slice(c.Pages, func(i, j int) bool { return naturalLess(c.Pages[i].Name, c.Pages[j].Name) })
	return c, nil
}

func (c *CBZ) Close() error { return c.zr.Close() }

var numRe = regexp.MustCompile(`\d+`)

// naturalLess ordena "… 2.webp" antes que "… 10.webp".
func naturalLess(a, b string) bool {
	ka := numRe.ReplaceAllStringFunc(a, func(s string) string { return fmt.Sprintf("%012s", s) })
	kb := numRe.ReplaceAllStringFunc(b, func(s string) string { return fmt.Sprintf("%012s", s) })
	return ka < kb
}

// CleanParts borra restos de escrituras interrumpidas (*.cbz.part) bajo root.
func CleanParts(root string) (n int) {
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".cbz.part") {
			if os.Remove(p) == nil {
				n++
			}
		}
		return nil
	})
	return
}
