package hostproc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// El binario de test hace también de host falso: con FAKE_HOST=1 habla el protocolo por stdin/stdout.
func TestMain(m *testing.M) {
	if os.Getenv("FAKE_HOST") == "1" {
		fakeHost()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeHost() {
	out := bufio.NewWriter(os.Stdout)
	var mu sync.Mutex
	send := func(v any) {
		b, _ := json.Marshal(v)
		mu.Lock()
		out.Write(append(b, '\n'))
		out.Flush()
		mu.Unlock()
	}
	switch os.Getenv("FAKE_MODE") {
	case "fatal":
		send(map[string]any{"event": "fatal", "error": map[string]any{"kind": "host", "message": "sin Java"}})
		os.Exit(2)
	case "garbage":
		fmt.Println("esto no es JSON")
		os.Exit(1)
	case "oomstart":
		if os.Getenv("FAKE_BOOST") != "1" {
			os.Exit(3)
		}
	}
	send(map[string]any{"event": "ready", "protocol": 1, "init_ms": 7})
	var loads atomic.Int32
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<20)
	for in.Scan() {
		var req map[string]any
		json.Unmarshal(in.Bytes(), &req)
		id := req["id"]
		switch req["op"] {
		case "quit":
			return
		case "load":
			loads.Add(1)
			send(map[string]any{"id": id, "ok": true, "result": map[string]any{"sources": []any{}}})
		case "ping":
			send(map[string]any{"id": id, "ok": true, "result": map[string]any{"pong": true, "loads": loads.Load(), "pid": os.Getpid(), "boost": os.Getenv("FAKE_BOOST") == "1"}})
		case "sleep":
			go func(id any, ms float64) {
				time.Sleep(time.Duration(ms) * time.Millisecond)
				send(map[string]any{"id": id, "ok": true, "result": map[string]any{"slept": ms}})
			}(id, req["ms"].(float64))
		case "fail":
			send(map[string]any{"id": id, "ok": false, "error": map[string]any{"kind": "cloudflare", "message": "desafío", "http": 403}})
		case "crash":
			os.Exit(1)
		case "oom":
			if os.Getenv("FAKE_BOOST") == "1" {
				send(map[string]any{"id": id, "ok": true, "result": map[string]any{"boosted": true}})
			} else {
				os.Exit(3) // -XX:+ExitOnOutOfMemoryError
			}
		case "mem":
			send(map[string]any{"id": id, "ok": true, "result": map[string]any{}})
		}
	}
}

func newSup(t *testing.T, mode string, opt Options) *Supervisor {
	t.Helper()
	exe, _ := os.Executable()
	s := New(opt)
	s.SetCommand(func(boost bool) []string {
		os.Setenv("FAKE_HOST", "1") // lo hereda el hijo (cmd.Env parte de os.Environ())
		os.Setenv("FAKE_MODE", mode)
		os.Setenv("FAKE_BOOST", map[bool]string{true: "1", false: "0"}[boost])
		return []string{exe, "-test.run=^$"}
	})
	t.Cleanup(func() {
		s.Close()
		os.Unsetenv("FAKE_HOST")
	})
	return s
}

func ping(t *testing.T, s *Supervisor, jar string) map[string]any {
	t.Helper()
	res, err := s.CallJar(context.Background(), jar, "ping", nil)
	if err != nil {
		t.Fatalf("ping: %v", err)
	}
	var m map[string]any
	json.Unmarshal(res, &m)
	return m
}

func TestCallsConcurrent(t *testing.T) {
	s := newSup(t, "", Options{})
	var wg sync.WaitGroup
	for i := 5; i >= 1; i-- { // la más lenta primero: las respuestas llegan desordenadas
		wg.Add(1)
		go func(ms int) {
			defer wg.Done()
			res, err := s.Call(context.Background(), "sleep", map[string]any{"ms": ms * 20})
			var m map[string]float64
			json.Unmarshal(res, &m)
			if err != nil || m["slept"] != float64(ms*20) {
				t.Errorf("sleep %d: %v %v", ms*20, m, err)
			}
		}(i)
	}
	wg.Wait()
	if st := s.Stats(); st.Starts != 1 || !st.Running {
		t.Errorf("arranques %d, vivo %v", st.Starts, st.Running)
	}
	_, err := s.Call(context.Background(), "fail", nil)
	var he *Error
	if !errors.As(err, &he) || he.Kind != "cloudflare" || he.HTTP != 403 {
		t.Errorf("error de la extensión: %#v", err)
	}
}

// Si el host se cae en mitad de una orden, la orden falla con «host» y la siguiente lo arranca de nuevo y vuelve
// a cargar el jar.
func TestCrashRestartsAndReloads(t *testing.T) {
	exits := make(chan bool, 4)
	s := newSup(t, "", Options{OnExit: func(clean bool) { exits <- clean }})
	first := ping(t, s, "/x.jar")
	if first["loads"].(float64) != 1 {
		t.Fatalf("el jar no se cargó: %v", first)
	}
	_, err := s.CallJar(context.Background(), "/x.jar", "crash", nil)
	if Kind(err) != "host" {
		t.Fatalf("caída: %v", err)
	}
	if clean := <-exits; clean {
		t.Error("una caída se tomó por apagado limpio")
	}
	second := ping(t, s, "/x.jar")
	if second["pid"] == first["pid"] || second["loads"].(float64) != 1 {
		t.Errorf("tras la caída: %v (antes %v)", second, first)
	}
	if st := s.Stats(); st.Crashes != 1 || st.Starts != 2 {
		t.Errorf("caídas %d, arranques %d", st.Crashes, st.Starts)
	}
}

// Una orden que agota la memoria se repite una vez con el heap ampliado (y los arranques siguientes van ampliados).
func TestOOMRetriesBoosted(t *testing.T) {
	ooms := 0
	s := newSup(t, "", Options{BoostFor: time.Hour, OnOOM: func() { ooms++ }})
	res, err := s.Call(context.Background(), "oom", nil)
	if err != nil || string(res) != `{"boosted":true}` {
		t.Fatalf("tras OOM: %s %v", res, err)
	}
	if ooms != 1 || !s.Stats().Boosted {
		t.Errorf("OOM avisados %d, ampliado %v", ooms, s.Stats().Boosted)
	}
}

func TestStartupFailures(t *testing.T) {
	for mode, want := range map[string]string{"fatal": "sin Java", "garbage": "esto no es JSON"} {
		s := newSup(t, mode, Options{})
		_, err := s.Call(context.Background(), "ping", nil)
		var he *Error
		if !errors.As(err, &he) || he.Kind != "host" || !contains(he.Message, want) {
			t.Errorf("%s: %v", mode, err)
		}
	}
	// sin memoria al arrancar: el siguiente intento arranca ampliado y funciona
	s := newSup(t, "oomstart", Options{BoostFor: time.Hour})
	if m := ping(t, s, ""); m["boost"] != true {
		t.Errorf("tras OOM al arrancar: %v", m)
	}
}

// Una orden que tarda más que su plazo falla, pero el host sigue vivo para las siguientes. El fallo es de la web
// (se colgó), no del host: así la descarga espera cada vez más en vez de reintentar cada 3 s.
func TestTimeoutKeepsHost(t *testing.T) {
	s := newSup(t, "", Options{})
	ping(t, s, "")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	_, err := s.Call(ctx, "sleep", map[string]any{"ms": 2000})
	cancel()
	if err == nil || Kind(err) != "network" {
		t.Fatalf("plazo agotado: %v (kind %s)", err, Kind(err))
	}
	if st := s.Stats(); st.Starts != 1 {
		t.Errorf("el host se reinició: %d arranques", st.Starts)
	}
	ping(t, s, "")
}

func TestKeepAliveAndClose(t *testing.T) {
	s := newSup(t, "", Options{KeepAlive: 100 * time.Millisecond})
	ping(t, s, "")
	deadline := time.Now().Add(6 * time.Second)
	for s.Stats().Running && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if s.Stats().Running {
		t.Fatal("el host no se apagó tras el keep-alive")
	}
	ping(t, s, "") // vuelve a arrancar solo
	s.Close()
	if _, err := s.Call(context.Background(), "ping", nil); Kind(err) != "host" {
		t.Errorf("tras cerrar: %v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
