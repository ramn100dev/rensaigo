// Package logx: escritor de logs con rotación por tamaño. Guarda el fichero actual y UNO anterior (.1), nada más.
package logx

import (
	"os"
	"sync"
)

type Rotating struct {
	mu   sync.Mutex
	path string
	max  int64
	f    *os.File
	size int64
}

func NewRotating(path string, maxBytes int64) (*Rotating, error) {
	r := &Rotating{path: path, max: maxBytes}
	return r, r.open()
}

func (r *Rotating) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	st, _ := f.Stat()
	r.f, r.size = f, st.Size()
	return nil
}

func (r *Rotating) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size+int64(len(p)) > r.max {
		r.f.Close()
		os.Rename(r.path, r.path+".1") // sustituye al anterior: el total nunca pasa de 2×max
		if err := r.open(); err != nil {
			return 0, err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}
