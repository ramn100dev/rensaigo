// Package hostproc supervisa el host JVM de extensiones: lo arranca bajo demanda, multiplexa órdenes
// por stdin/stdout (JSON por líneas), lo apaga tras un tiempo sin uso y lo reinicia si se cae.
package hostproc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"rensaigo/internal/i18n"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Error devuelto por el host (o por el supervisor, con Kind "host").
type Error struct {
	Kind    string `json:"kind"` // cloudflare|webview|javascript|http|network|extension|host
	Message string `json:"message"`
	HTTP    int    `json:"http,omitempty"`
	OOM     bool   `json:"-"` // el host murió por OutOfMemoryError durante la orden
}

func (e *Error) Error() string { return e.Kind + ": " + e.Message }

// Kind extrae la clase de un error cualquiera.
func Kind(err error) string {
	var he *Error
	if errors.As(err, &he) {
		return he.Kind
	}
	return "host"
}

type Options struct {
	Argv      []string      // ejecutable y argumentos (sin shell: la imagen final puede no tener sh)
	Env       []string      // variables extra
	KeepAlive time.Duration // apagado tras este tiempo sin órdenes
	Stderr    io.Writer
	OnExit    func(clean bool) // aviso al núcleo
	OnOOM     func()           // el host salió por falta de memoria (se reintenta con más heap)
	BoostFor  time.Duration    // tras un OutOfMemoryError, cuánto tiempo arrancan los siguientes hosts con el heap ampliado
	IdleGC    bool             // pedir un GC completo al host cuando lleva unos segundos sin órdenes (devuelve heap al sistema)
	RecycleKb int64            // si tras el GC el host sigue por encima de este RSS, se apaga (la siguiente orden lo arranca limpio)
}

type Stats struct {
	Running     bool   `json:"running"`
	PID         int    `json:"pid"`
	Starts      int    `json:"starts"`
	Crashes     int    `json:"crashes"`
	Recycles    int    `json:"recycles"`
	OOMs        int    `json:"ooms"`
	Boosted     bool   `json:"boosted"`       // el proceso actual usa el heap ampliado
	LastReadyMs int64  `json:"last_ready_ms"` // desde el exec hasta la línea "ready"
	InitMs      int64  `json:"init_ms"`       // lo que reporta el propio host
	RSSKb       int64  `json:"rss_kb"`
	HWMKb       int64  `json:"hwm_kb"`
	PeakHWMKb   int64  `json:"peak_hwm_kb"` // máximo observado entre todos los arranques
	InFlight    int32  `json:"in_flight"`
	IdleFor     string `json:"idle_for"`
	LoadedJars  int    `json:"loaded_jars"`
}

type reply struct {
	ID     int64           `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  *Error          `json:"error"`
	Ms     int64           `json:"ms"`
}

type proc struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	wmu      sync.Mutex
	pmu      sync.Mutex
	pending  map[int64]chan reply
	dead     chan struct{}
	lmu      sync.Mutex
	loaded   map[string]json.RawMessage // jar -> resultado de "load"
	started  time.Time
	boosted  bool
	oom      atomic.Bool
	stopping atomic.Bool
}

type Supervisor struct {
	opt      Options
	mu       sync.Mutex // protege p y el arranque
	p        *proc
	nextID   atomic.Int64
	inFlight atomic.Int32
	lastUse  atomic.Int64
	starts   atomic.Int64
	crashes  atomic.Int64
	readyMs  atomic.Int64
	initMs   atomic.Int64
	peakHWM  atomic.Int64
	closed   atomic.Bool
	gcDone   atomic.Bool
	recycles atomic.Int64
	ooms     atomic.Int64
	boostTil atomic.Int64 // hasta cuándo (UnixNano) se arranca con heap ampliado
	cmdFn    func(boost bool) []string
}

// SetCommand permite recalcular la línea de órdenes en cada arranque (p. ej. si ya existe la caché AOT).
func (s *Supervisor) SetCommand(fn func(boost bool) []string) { s.cmdFn = fn }

func New(opt Options) *Supervisor {
	s := &Supervisor{opt: opt}
	s.lastUse.Store(time.Now().UnixNano())
	go s.idleLoop()
	return s
}

func (s *Supervisor) idleLoop() {
	for !s.closed.Load() {
		time.Sleep(2 * time.Second)
		s.sampleMem()
		s.idleTrim()
		if s.opt.KeepAlive <= 0 || s.inFlight.Load() > 0 {
			continue
		}
		if time.Since(time.Unix(0, s.lastUse.Load())) < s.opt.KeepAlive {
			continue
		}
		s.mu.Lock()
		p := s.p
		// se vuelve a comprobar con el candado: puede haber entrado una orden
		if p != nil && s.inFlight.Load() == 0 && time.Since(time.Unix(0, s.lastUse.Load())) >= s.opt.KeepAlive {
			s.p = nil
			s.mu.Unlock()
			s.stop(p)
			continue
		}
		s.mu.Unlock()
	}
}

// idleTrim: cuando el host lleva 4 s sin órdenes se le pide un GC completo (una vez por periodo de inactividad).
// El reciclado por tamaño NO se hace aquí sino al llegar la siguiente orden (recycleIfBloated): en la prueba de 3 h,
// reciclar en reposo reiniciaba el host tras cada comprobación (213 veces) y anulaba el keep-alive.
func (s *Supervisor) idleTrim() {
	if !s.opt.IdleGC {
		return
	}
	if s.inFlight.Load() > 0 || time.Since(time.Unix(0, s.lastUse.Load())) < 4*time.Second {
		return
	}
	s.mu.Lock()
	p := s.p
	s.mu.Unlock()
	if p == nil {
		return
	}
	if s.gcDone.CompareAndSwap(false, true) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		s.call(ctx, p, "mem", map[string]any{"gc": true})
		cancel()
	}
}

func (s *Supervisor) sampleMem() {
	s.mu.Lock()
	p := s.p
	s.mu.Unlock()
	if p == nil {
		return
	}
	_, hwm := procMem(p.cmd.Process.Pid)
	for {
		old := s.peakHWM.Load()
		if hwm <= old || s.peakHWM.CompareAndSwap(old, hwm) {
			break
		}
	}
}

func procMem(pid int) (rss, hwm int64) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return
	}
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

func (s *Supervisor) stop(p *proc) {
	p.stopping.Store(true)
	s.sampleMemOf(p)
	p.wmu.Lock()
	io.WriteString(p.stdin, "{\"op\":\"quit\"}\n")
	p.stdin.Close()
	p.wmu.Unlock()
	select {
	case <-p.dead:
	case <-time.After(8 * time.Second):
		p.cmd.Process.Kill()
		<-p.dead
	}
}

func (s *Supervisor) sampleMemOf(p *proc) {
	_, hwm := procMem(p.cmd.Process.Pid)
	if hwm > s.peakHWM.Load() {
		s.peakHWM.Store(hwm)
	}
}

// Stop apaga el host ahora (si está vivo).
func (s *Supervisor) Stop() {
	s.mu.Lock()
	p := s.p
	s.p = nil
	s.mu.Unlock()
	if p != nil {
		s.stop(p)
	}
}

func (s *Supervisor) Close() { s.closed.Store(true); s.Stop() }

func (s *Supervisor) ensure(ctx context.Context) (*proc, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.p != nil {
		select {
		case <-s.p.dead:
			s.p = nil
		default:
			return s.p, nil
		}
	}
	if s.closed.Load() {
		return nil, &Error{Kind: "host", Message: i18n.T("supervisor cerrado")}
	}
	t0 := time.Now()
	argv := s.opt.Argv
	boost := time.Now().UnixNano() < s.boostTil.Load()
	if s.cmdFn != nil {
		argv = s.cmdFn(boost)
	}
	if len(argv) == 0 {
		return nil, &Error{Kind: "host", Message: i18n.T("comando del host sin configurar")}
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), s.opt.Env...)
	cmd.Stderr = s.opt.Stderr
	// Grupo propio: si el núcleo recibe SIGINT en un terminal, el host no lo recibe directamente;
	// Pdeathsig: si el núcleo muere, el kernel mata al host (nunca quedan java colgados).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, &Error{Kind: "host", Message: i18n.Tf("no se pudo arrancar el host: %v", err)}
	}
	p := &proc{cmd: cmd, stdin: stdin, pending: map[int64]chan reply{}, dead: make(chan struct{}), loaded: map[string]json.RawMessage{}, started: time.Now(), boosted: boost}
	rd := bufio.NewReaderSize(stdout, 64<<10)
	ready := make(chan error, 1)
	go func() {
		first := true
		for {
			line, err := rd.ReadBytes('\n')
			if len(line) > 0 {
				if first {
					first = false
					var ev struct {
						Event  string `json:"event"`
						InitMs int64  `json:"init_ms"`
						Error  *Error `json:"error"`
					}
					if json.Unmarshal(line, &ev) != nil || ev.Event != "ready" {
						msg := strings.TrimSpace(string(line))
						if ev.Error != nil {
							msg = ev.Error.Message
						}
						ready <- &Error{Kind: "host", Message: i18n.Tf("el host no arrancó: %s", msg)}
					} else {
						s.initMs.Store(ev.InitMs)
						ready <- nil
					}
				} else {
					var r reply
					if json.Unmarshal(line, &r) == nil {
						p.pmu.Lock()
						ch := p.pending[r.ID]
						delete(p.pending, r.ID)
						p.pmu.Unlock()
						if ch != nil {
							ch <- r
						}
					}
				}
			}
			if err != nil {
				break
			}
		}
		if first {
			ready <- &Error{Kind: "host", Message: i18n.T("el host terminó sin dar señal de listo")}
		}
		cmd.Wait()
		clean := p.stopping.Load()
		// -XX:+ExitOnOutOfMemoryError hace que la JVM salga con código 3 al primer OutOfMemoryError
		oom := !clean && cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 3
		if !clean {
			s.crashes.Add(1)
			log.Printf("host: el proceso %d terminó de forma inesperada (%v)", cmd.Process.Pid, cmd.ProcessState)
		}
		msg := i18n.T("el host murió durante la orden")
		if oom {
			p.oom.Store(true)
			s.ooms.Add(1)
			if s.opt.BoostFor > 0 {
				s.boostTil.Store(time.Now().Add(s.opt.BoostFor).UnixNano())
			}
			msg = i18n.T("el host se quedó sin memoria (OutOfMemoryError)")
			if s.opt.OnOOM != nil {
				s.opt.OnOOM()
			}
		}
		p.pmu.Lock()
		for id, ch := range p.pending {
			ch <- reply{ID: id, Error: &Error{Kind: "host", Message: msg, OOM: oom}}
			delete(p.pending, id)
		}
		p.pmu.Unlock()
		close(p.dead)
		if s.opt.OnExit != nil {
			s.opt.OnExit(clean)
		}
	}()
	select {
	case err := <-ready:
		if err != nil {
			cmd.Process.Kill()
			<-p.dead // espera a que se procese la salida: si fue por falta de memoria, el siguiente arranque va ampliado
			if p.oom.Load() {
				return nil, &Error{Kind: "host", Message: i18n.T("el host se quedó sin memoria al arrancar (OutOfMemoryError)"), OOM: true}
			}
			return nil, err
		}
	case <-time.After(45 * time.Second):
		cmd.Process.Kill()
		return nil, &Error{Kind: "host", Message: i18n.T("el host no respondió en 45 s")}
	case <-ctx.Done():
		cmd.Process.Kill()
		return nil, ctx.Err()
	}
	s.starts.Add(1)
	s.readyMs.Store(time.Since(t0).Milliseconds())
	s.p = p
	return p, nil
}

func (s *Supervisor) call(ctx context.Context, p *proc, op string, args map[string]any) (json.RawMessage, error) {
	id := s.nextID.Add(1)
	req := map[string]any{"id": id, "op": op}
	for k, v := range args {
		req[k] = v
	}
	b, _ := json.Marshal(req)
	ch := make(chan reply, 1)
	p.pmu.Lock()
	select {
	case <-p.dead:
		p.pmu.Unlock()
		return nil, &Error{Kind: "host", Message: i18n.T("el host no está en marcha")}
	default:
	}
	p.pending[id] = ch
	p.pmu.Unlock()
	p.wmu.Lock()
	_, err := p.stdin.Write(append(b, '\n'))
	p.wmu.Unlock()
	if err != nil {
		p.pmu.Lock()
		delete(p.pending, id)
		p.pmu.Unlock()
		return nil, &Error{Kind: "host", Message: i18n.Tf("escritura al host: %v", err)}
	}
	select {
	case r := <-ch:
		if r.Error != nil {
			return nil, r.Error
		}
		return r.Result, nil
	case <-ctx.Done():
		p.pmu.Lock()
		delete(p.pending, id)
		p.pmu.Unlock()
		return nil, &Error{Kind: "host", Message: i18n.Tf("tiempo de espera agotado (%s)", op)}
	}
}

// Call ejecuta una orden sin source (ping, mem, cookies).
func (s *Supervisor) Call(ctx context.Context, op string, args map[string]any) (json.RawMessage, error) {
	return s.do(ctx, "", op, args)
}

// CallJar ejecuta una orden asegurando antes que el jar está cargado en el proceso actual del host.
func (s *Supervisor) CallJar(ctx context.Context, jar, op string, args map[string]any) (json.RawMessage, error) {
	return s.do(ctx, jar, op, args)
}

// recycleIfBloated: en la frontera entre trabajos (ninguna orden en curso) apaga el host si ha engordado por encima
// de RecycleKb; la orden que llega arranca uno limpio (~0,5 s). Así un proceso que acaba de comprobar muchos sources
// no arrastra esa memoria a las descargas siguientes. No recicla procesos de menos de 30 s para no entrar en bucle.
func (s *Supervisor) recycleIfBloated() {
	if s.opt.RecycleKb <= 0 {
		return
	}
	s.mu.Lock()
	p := s.p
	// un proceso con heap ampliado no se recicla por tamaño: se apagará por inactividad
	if p == nil || p.boosted || s.inFlight.Load() != 0 || time.Since(p.started) < 30*time.Second {
		s.mu.Unlock()
		return
	}
	rss, _ := procMem(p.cmd.Process.Pid)
	if rss <= s.opt.RecycleKb {
		s.mu.Unlock()
		return
	}
	s.p = nil
	s.mu.Unlock()
	s.recycles.Add(1)
	log.Printf("host: reciclado entre trabajos (%d MB > %d MB)", rss/1024, s.opt.RecycleKb/1024)
	s.stop(p)
}

func (s *Supervisor) do(ctx context.Context, jar, op string, args map[string]any) (json.RawMessage, error) {
	s.recycleIfBloated()
	s.inFlight.Add(1)
	s.gcDone.Store(false)
	s.lastUse.Store(time.Now().UnixNano())
	defer func() {
		s.lastUse.Store(time.Now().UnixNano())
		s.inFlight.Add(-1)
	}()
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 4*time.Minute)
		defer cancel()
	}
	res, err := s.attempt(ctx, jar, op, args)
	var he *Error
	if errors.As(err, &he) && he.OOM && s.opt.BoostFor > 0 {
		// El host se quedó sin memoria con esta orden (p. ej. una serie de más de mil capítulos): se repite UNA vez;
		// el host nuevo arranca ya con el heap ampliado. No es un fallo del source.
		log.Printf("host: %s se repite con heap ampliado tras OutOfMemoryError", op)
		res, err = s.attempt(ctx, jar, op, args)
	}
	return res, err
}

func (s *Supervisor) attempt(ctx context.Context, jar, op string, args map[string]any) (json.RawMessage, error) {
	p, err := s.ensure(ctx)
	if err != nil {
		return nil, err
	}
	if jar != "" {
		p.lmu.Lock()
		res, ok := p.loaded[jar]
		if !ok {
			res, err = s.call(ctx, p, "load", map[string]any{"jar": jar})
			if err == nil {
				p.loaded[jar] = res
			}
		}
		p.lmu.Unlock()
		if err != nil {
			return nil, err
		}
		if op == "load" {
			return res, nil
		}
	}
	return s.call(ctx, p, op, args)
}

func (s *Supervisor) Stats() Stats {
	st := Stats{
		Starts: int(s.starts.Load()), Crashes: int(s.crashes.Load()), Recycles: int(s.recycles.Load()), OOMs: int(s.ooms.Load()), LastReadyMs: s.readyMs.Load(), InitMs: s.initMs.Load(),
		InFlight: s.inFlight.Load(), IdleFor: time.Since(time.Unix(0, s.lastUse.Load())).Round(time.Second).String(),
	}
	s.mu.Lock()
	p := s.p
	s.mu.Unlock()
	if p != nil {
		select {
		case <-p.dead:
		default:
			st.Running = true
			st.Boosted = p.boosted
			st.PID = p.cmd.Process.Pid
			st.RSSKb, st.HWMKb = procMem(st.PID)
			p.lmu.Lock()
			st.LoadedJars = len(p.loaded)
			p.lmu.Unlock()
			if st.HWMKb > s.peakHWM.Load() {
				s.peakHWM.Store(st.HWMKb)
			}
		}
	}
	st.PeakHWMKb = s.peakHWM.Load()
	return st
}

// ResetPeak pone a cero el pico observado (para medir un tramo concreto).
func (s *Supervisor) ResetPeak() { s.peakHWM.Store(0) }

var _ = fmt.Sprintf
