package api

import (
	"context"
	"sync"
	"testing"

	"rensaigo/internal/track"
)

// fakeTrk: un servicio de seguimiento en memoria (una sola ficha, «1»).
type fakeTrk struct {
	mu     sync.Mutex
	read   int
	total  int
	pushes []int
}

func (f *fakeTrk) Key() string              { return "mal" }
func (f *fakeTrk) Name() string             { return "MyAnimeList" }
func (f *fakeTrk) Connected() bool          { return true }
func (f *fakeTrk) User() string             { return "yo" }
func (f *fakeTrk) Disconnect()              {}
func (f *fakeTrk) Statuses() []track.Status { return nil }
func (f *fakeTrk) Search(context.Context, string) ([]track.Entry, error) {
	return []track.Entry{{ID: "1", Title: "Test Series"}}, nil
}
func (f *fakeTrk) List(context.Context, string) ([]track.Entry, error) { return nil, nil }
func (f *fakeTrk) Get(context.Context, string) (*track.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &track.Entry{ID: "1", Title: "Test Series", MyRead: f.read, Total: f.total}, nil
}
func (f *fakeTrk) Push(_ context.Context, _ string, read, _ int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.read = read
	f.pushes = append(f.pushes, read)
	return nil
}
func (f *fakeTrk) state() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.read, len(f.pushes)
}

func TestTrackSync(t *testing.T) {
	env := newTestEnv(t) // serie con capítulos 1, 2 y 3; vinculada a «mal» con rd=1
	s := env.s
	f := &fakeTrk{read: 1, total: 3}
	s.Trk = []track.Service{f}
	ids := map[int]int64{}
	for _, r := range s.rows(`SELECT id, number FROM chapter WHERE series_id=?`, env.series) {
		ids[int(toF(r["number"]))] = r["id"].(int64)
	}
	read := func(n int) bool {
		var d int
		s.St.DB.QueryRow(`SELECT coalesce((SELECT done FROM progress WHERE chapter_id=?),0)`, ids[n]).Scan(&d)
		return d == 1
	}
	mark := func(n int) {
		s.St.DB.Exec(`INSERT INTO progress(chapter_id,page,done,updated) VALUES(?,0,1,1) ON CONFLICT(chapter_id) DO UPDATE SET done=1`, ids[n])
	}

	// terminas el 2 aquí: se sube 2
	mark(1)
	mark(2)
	s.syncSeries(env.series, false)
	if r, _ := f.state(); r != 2 {
		t.Fatalf("tras leer el 2, el servicio tiene %d", r)
	}
	// en el móvil vas por el 3: la pasada diaria lo trae (y no lo pisa con el 2 de aquí)
	f.mu.Lock()
	f.read = 3
	f.mu.Unlock()
	_, before := f.state()
	s.trackSyncAll(context.Background())
	if r, n := f.state(); r != 3 || n != before {
		t.Fatalf("lo del servicio se pisó: %d (subidas %d→%d)", r, before, n)
	}
	if !read(3) {
		t.Fatal("el 3, leído en el servicio, no se marcó aquí")
	}
	// desmarcas el 3 a mano: el servicio baja a 2
	s.St.DB.Exec(`DELETE FROM progress WHERE chapter_id=?`, ids[3])
	s.syncSeries(env.series, true)
	if r, _ := f.state(); r != 2 {
		t.Fatalf("desmarcar a mano no bajó el servicio: %d", r)
	}
	// y la pasada diaria no lo vuelve a marcar con el valor viejo
	s.trackSyncAll(context.Background())
	if read(3) {
		t.Fatal("la pasada diaria volvió a marcar el 3")
	}
	// una pasada automática nunca baja el contador
	s.St.DB.Exec(`DELETE FROM progress WHERE chapter_id=?`, ids[2])
	s.syncSeries(env.series, false)
	if r, _ := f.state(); r != 2 {
		t.Fatalf("un proceso automático bajó el contador a %d", r)
	}
	// terminas uno aquí mientras en el servicio ya ibas más adelante: se trae lo del servicio en vez de bajarlo
	s.St.DB.Exec(`DELETE FROM progress`)
	s.St.DB.Exec(`UPDATE tracking SET rd=0 WHERE series_id=?`, env.series)
	f.mu.Lock()
	f.read = 3
	f.mu.Unlock()
	mark(1)
	s.syncSeries(env.series, false)
	if r, _ := f.state(); r != 3 || !read(2) || !read(3) {
		t.Fatalf("leer el 1 con el servicio en el 3: servicio %d, 2 leído=%v, 3 leído=%v", r, read(2), read(3))
	}
	// nunca se sube más que el total
	f.mu.Lock()
	f.total, f.read = 2, 1
	f.mu.Unlock()
	s.St.DB.Exec(`UPDATE tracking SET total=2, rd=1 WHERE series_id=?`, env.series)
	mark(2)
	mark(3)
	s.syncSeries(env.series, false)
	if r, _ := f.state(); r != 2 {
		t.Fatalf("se subió %d con un total de 2", r)
	}
}
