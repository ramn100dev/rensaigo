// Package jsengine: capa OPCIONAL de JavaScript (GraalJS) para las ~30 extensiones que usan QuickJs.
// No va en la imagen: se descarga bajo demanda de Maven Central a /config/jsengine, verificando el sha256 fijado de
// cada jar. Con los jars presentes, el host arranca con ellos en el classpath; GraalJS solo se carga (y solo gasta
// memoria, ~87 MB medidos) cuando una extensión ejecuta JavaScript de verdad.
package jsengine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const version = "25.3.4.1" // la misma que fija Suwayomi v2.4.2366 (libs.versions.toml: polyglot)

type artifact struct{ group, name, sha256 string }

// Lista exacta que resuelve Gradle para org.graalvm.polyglot:js-community (host/build.sh -PwithGraalJs).
var artifacts = []artifact{
	{"org/graalvm/sdk", "collections", "3d0d2287f0217e7e765a47d69681c0ac1bb4ef50d70c9440449e86045bbe158f"},
	{"org/graalvm/shadowed", "icu4j", "dc408acbde92cb9233e184ee4ec135185dbcf1f53013e6356c500a6ff50a2909"},
	{"org/graalvm/sdk", "jniutils", "b1d78402c744e5022fcb19f620644ab38b834c024020f3570dadaaf9cbe77067"},
	{"org/graalvm/js", "js-language", "32a9e6b9b6b74d2af0f8772190dfd2d16e67134e69ce10e6e3521e7f084070b6"},
	{"org/graalvm/sdk", "nativeimage", "a4315500775e98d16f083812bf5b8b664d84abad736da14ebbea5b38d7752b0f"},
	{"org/graalvm/polyglot", "polyglot", "befec2cbfb6ca279a73ee20626718a917ab1d1a3970793698ddc11b4d41e0356"},
	{"org/graalvm/regex", "regex", "7a8d81397939e693eaf089ce75c6395255e3b6896e4578e6fffb6ed458fa43e2"},
	{"org/graalvm/truffle", "truffle-api", "f9efa18b1cb6c4e39862f5ac83a88ee4c9e31252c52c2ee5192c2a707cd9b27f"},
	{"org/graalvm/truffle", "truffle-compiler", "a422df88ffaabe4669b8b6e5d63f04f2a8152e5fe763b3bfbf3dc58d7e5dd41c"},
	{"org/graalvm/truffle", "truffle-runtime", "d74ff0b8eb9a5fec4a571f78bc21750ee41dfd454877b8cce8b33641efd90ae3"},
	{"org/graalvm/sdk", "word", "fe1c3723e27907789732ac9f3d639ba6055d459529cfcfbd96156e73911ee7b7"},
	{"org/graalvm/shadowed", "xz", "876df268932813d3d5a5346ca138ce9b333560076eeb71e9e5b7bcb647a001e7"},
}

type Engine struct{ Dir string }

func (a artifact) file() string { return a.name + "-" + version + ".jar" }

// Installed: están todos los jars (la marca .ok se escribe solo tras verificarlos todos).
func (e *Engine) Installed() bool {
	_, err := os.Stat(filepath.Join(e.Dir, ".ok"))
	return err == nil
}

// Classpath devuelve los jars en orden, separados por ':' (vacío si no está instalado).
func (e *Engine) Classpath() string {
	if !e.Installed() {
		return ""
	}
	var parts []string
	for _, a := range artifacts {
		parts = append(parts, filepath.Join(e.Dir, a.file()))
	}
	return strings.Join(parts, ":")
}

func (e *Engine) Size() (n int64) {
	entries, _ := os.ReadDir(e.Dir)
	for _, f := range entries {
		if in, err := f.Info(); err == nil {
			n += in.Size()
		}
	}
	return
}

func (e *Engine) Remove() error { return os.RemoveAll(e.Dir) }

// Install descarga y verifica cada jar. Si algo falla no queda instalado a medias.
func (e *Engine) Install(ctx context.Context) error {
	if err := os.MkdirAll(e.Dir, 0o755); err != nil {
		return err
	}
	os.Remove(filepath.Join(e.Dir, ".ok"))
	client := &http.Client{Timeout: 10 * time.Minute}
	for _, a := range artifacts {
		dest := filepath.Join(e.Dir, a.file())
		if sum, err := fileSHA(dest); err == nil && sum == a.sha256 {
			continue
		}
		url := fmt.Sprintf("https://repo1.maven.org/maven2/%s/%s/%s/%s", a.group, a.name, version, a.file())
		req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		if resp.StatusCode != 200 {
			resp.Body.Close()
			return fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
		}
		tmp := dest + ".part"
		f, err := os.Create(tmp)
		if err != nil {
			resp.Body.Close()
			return err
		}
		h := sha256.New()
		_, err = io.Copy(io.MultiWriter(f, h), resp.Body)
		resp.Body.Close()
		f.Close()
		if err != nil {
			os.Remove(tmp)
			return err
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != a.sha256 {
			os.Remove(tmp)
			return fmt.Errorf("%s: sha256 inesperado (%s)", a.file(), got)
		}
		if err := os.Rename(tmp, dest); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(e.Dir, ".ok"), []byte(version+"\n"), 0o644)
}

func fileSHA(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
