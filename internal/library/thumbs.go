package library

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"sync"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// Thumbs genera miniaturas bajo demanda, DE UNA EN UNA, con caché en disco de tamaño acotado.
// Decodificar una página (p. ej. 1500×2200) cuesta varios MB; por eso hay un único candado global,
// se rechazan imágenes desmesuradas y tras cada generación se devuelve la memoria al sistema.
type Thumbs struct {
	Dir      string
	MaxBytes int64
	Width    int
	mu       sync.Mutex
	made     int
}

const maxPixels = 40_000_000

func (t *Thumbs) path(key string) string {
	h := sha1.Sum([]byte(key))
	return filepath.Join(t.Dir, hex.EncodeToString(h[:])+".jpg")
}

// Get devuelve la ruta de la miniatura de la primera página del CBZ, generándola si no existe.
func (t *Thumbs) Get(cbz string) (string, error) {
	st, err := os.Stat(cbz)
	if err != nil {
		return "", err
	}
	out := t.path(fmt.Sprintf("%s|%d|%d", cbz, st.Size(), st.ModTime().Unix()))
	if _, err := os.Stat(out); err == nil {
		return out, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, err := os.Stat(out); err == nil {
		return out, nil
	}
	if err := os.MkdirAll(t.Dir, 0o755); err != nil {
		return "", err
	}
	c, err := OpenCBZ(cbz, false)
	if err != nil {
		return "", err
	}
	defer c.Close()
	if len(c.Pages) == 0 {
		return "", errors.New("cbz sin páginas")
	}
	rc, err := c.Pages[0].Open()
	if err != nil {
		return "", err
	}
	cfg, _, err := image.DecodeConfig(rc)
	rc.Close()
	if err != nil {
		return "", fmt.Errorf("formato no soportado para miniatura: %w", err)
	}
	if cfg.Width*cfg.Height > maxPixels {
		return "", errors.New("imagen demasiado grande para miniatura")
	}
	rc, err = c.Pages[0].Open()
	if err != nil {
		return "", err
	}
	src, _, err := image.Decode(rc)
	rc.Close()
	if err != nil {
		return "", err
	}
	w := t.Width
	if w <= 0 {
		w = 300
	}
	h := src.Bounds().Dy() * w / src.Bounds().Dx()
	if h > w*2 { // tiras verticales (webtoon): se recorta la parte de arriba
		h = w * 3 / 2
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	sr := src.Bounds()
	if sr.Dy()*w > h*sr.Dx() {
		sr.Max.Y = sr.Min.Y + h*sr.Dx()/w
	}
	draw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, sr, draw.Src, nil)
	src = nil
	tmp := out + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	err = jpeg.Encode(f, dst, &jpeg.Options{Quality: 80})
	f.Close()
	if err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, out); err != nil {
		return "", err
	}
	dst = nil
	debug.FreeOSMemory()
	t.made++
	if t.made%20 == 1 {
		t.evict()
	}
	return out, nil
}

// evict borra las miniaturas más antiguas hasta quedar por debajo del límite.
func (t *Thumbs) evict() {
	entries, err := os.ReadDir(t.Dir)
	if err != nil {
		return
	}
	type fi struct {
		p    string
		size int64
		mod  int64
	}
	var all []fi
	var total int64
	for _, e := range entries {
		if in, err := e.Info(); err == nil && !e.IsDir() {
			all = append(all, fi{filepath.Join(t.Dir, e.Name()), in.Size(), in.ModTime().UnixNano()})
			total += in.Size()
		}
	}
	if total <= t.MaxBytes {
		return
	}
	sort.Slice(all, func(i, j int) bool { return all[i].mod < all[j].mod })
	for _, f := range all {
		if total <= t.MaxBytes*9/10 {
			break
		}
		if os.Remove(f.p) == nil {
			total -= f.size
		}
	}
}

// Size devuelve bytes y número de ficheros en la caché.
func (t *Thumbs) Size() (int64, int) {
	entries, _ := os.ReadDir(t.Dir)
	var total int64
	for _, e := range entries {
		if in, err := e.Info(); err == nil {
			total += in.Size()
		}
	}
	return total, len(entries)
}

var _ = io.EOF
