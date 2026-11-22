package api

import (
	"bytes"
	"context"
	"database/sql"
	"image"
	"image/jpeg"
	_ "image/png"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "golang.org/x/image/webp"

	"rensaigo/internal/library"
)

// Datos junto a cada serie: rensaigo.json (sources, lo leído, vínculos…) y cover.jpg. Los cambios se apuntan
// con touch y se escriben juntos cada 30 s, así leer un capítulo página a página no reescribe el fichero cada vez.

type dirtySet struct {
	mu  sync.Mutex
	ids map[int64]bool
}

// Touch: para el motor de descargas (OnChange).
func (s *Server) Touch(sid int64) { s.touch(sid) }

func (s *Server) touch(sid int64) {
	if sid <= 0 {
		return
	}
	s.dirty.mu.Lock()
	if s.dirty.ids == nil {
		s.dirty.ids = map[int64]bool{}
	}
	s.dirty.ids[sid] = true
	s.dirty.mu.Unlock()
}

// SidecarLoop escribe los datos pendientes. Al arrancar repasa todas las series (las de antes de esta función
// y las que se hayan quedado a medias).
func (s *Server) SidecarLoop(ctx context.Context) {
	for _, r := range s.rows(`SELECT id FROM series`) {
		s.touch(r["id"].(int64))
	}
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		s.flushSidecars(ctx)
		select {
		case <-ctx.Done():
			s.flushSidecars(context.Background()) // lo último antes de apagar
			return
		case <-t.C:
		}
	}
}

func (s *Server) flushSidecars(ctx context.Context) {
	s.dirty.mu.Lock()
	ids := s.dirty.ids
	s.dirty.ids = nil
	s.dirty.mu.Unlock()
	for id := range ids {
		if err := library.WriteSidecar(s.St.DB, id); err != nil && err != sql.ErrNoRows { // (borrada mientras tanto)
			log.Printf("datos de la serie %d: %v", id, err)
		}
		if ctx.Err() == nil {
			s.ensureCover(ctx, id)
		}
	}
}

// ensureCover deja un cover.jpg en la carpeta de la serie si no lo tiene: la portada oficial si está vinculada a
// un servicio, si no la del source y, como último recurso, la primera página. Si ya hay uno no se toca (puede
// haberlo puesto el usuario a mano).
func (s *Server) ensureCover(ctx context.Context, id int64) {
	var dir, thumb, base string
	if s.St.DB.QueryRow(`SELECT se.path, se.thumb_url, coalesce((SELECT src.base_url FROM series_source ss JOIN source src ON src.id=ss.source_id
		WHERE ss.series_id=se.id ORDER BY ss.priority LIMIT 1),'') FROM series se WHERE se.id=?`, id).Scan(&dir, &thumb, &base) != nil {
		return
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return
	}
	dest := filepath.Join(dir, "cover.jpg")
	if _, err := os.Stat(dest); err == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var img []byte
	if p := s.trackCoverFile(ctx, id); p != "" {
		img, _ = os.ReadFile(p)
	}
	if img == nil && thumb != "" {
		img, _ = fetchImage(ctx, thumb, base)
	}
	if img == nil {
		var cbz string
		s.St.DB.QueryRow(`SELECT cbz FROM chapter WHERE series_id=? AND state='downloaded' ORDER BY number LIMIT 1`, id).Scan(&cbz)
		if cbz == "" {
			return
		}
		p, err := s.Thumbs.Get(cbz)
		if err != nil {
			return
		}
		img, _ = os.ReadFile(p)
	}
	if img = asJPEG(img); img == nil {
		return
	}
	if os.WriteFile(dest+".tmp", img, 0o644) == nil {
		os.Rename(dest+".tmp", dest)
	}
}

// asJPEG devuelve la imagen en JPEG (convirtiendo PNG/WebP), o nil si no es una imagen válida o es enorme.
func asJPEG(b []byte) []byte {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil || cfg.Width*cfg.Height > 40_000_000 {
		return nil
	}
	if format == "jpeg" {
		return b
	}
	im, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return nil
	}
	var out bytes.Buffer
	if jpeg.Encode(&out, im, &jpeg.Options{Quality: 88}) != nil {
		return nil
	}
	return out.Bytes()
}
