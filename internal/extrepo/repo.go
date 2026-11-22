// Package extrepo: repositorio de extensiones (repo.json -> index.pb -> jarUrl), verificación e instalación.
package extrepo

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"rensaigo/internal/hostproc"
	"rensaigo/internal/store"
)

type Repo struct {
	St          *store.Store
	Host        *hostproc.Supervisor
	URL         string // repo.json
	Fingerprint string // huella fijada del certificado firmante
	Dir         string // donde se guardan los jars verificados
	HTTP        *http.Client
}

func New(st *store.Store, host *hostproc.Supervisor, repoURL, fingerprint, dir string) *Repo {
	return &Repo{St: st, Host: host, URL: repoURL, Fingerprint: strings.ToLower(fingerprint), Dir: dir,
		HTTP: &http.Client{Timeout: 90 * time.Second}}
}

func (r *Repo) get(ctx context.Context, u string, max int64) ([]byte, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	req.Header.Set("User-Agent", "rensaigo")
	resp, err := r.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GET %s: HTTP %d", u, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, max))
}

// Refresh descarga repo.json e index.pb, comprueba que la huella declarada es la fijada y guarda el índice.
func (r *Repo) Refresh(ctx context.Context) (int, error) {
	raw, err := r.get(ctx, r.URL, 1<<20)
	if err != nil {
		return 0, err
	}
	var rj struct {
		IndexV2 string `json:"index_v2"`
		Meta    struct {
			Name        string `json:"name"`
			Fingerprint string `json:"signingKeyFingerprint"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(raw, &rj); err != nil {
		return 0, fmt.Errorf("repo.json: %w", err)
	}
	// repo.json NO va firmado: lo único que se puede comprobar es que declara la huella que esperamos.
	// La garantía real es la firma de cada jar contra esa huella fijada (VerifyJar).
	if !strings.EqualFold(rj.Meta.Fingerprint, r.Fingerprint) {
		return 0, fmt.Errorf("repo.json declara la huella %q y se esperaba %q: repositorio rechazado", rj.Meta.Fingerprint, r.Fingerprint)
	}
	iu, err := url.Parse(rj.IndexV2)
	if err != nil || iu.Scheme != "https" {
		return 0, errors.New("repo.json: index_v2 inválido")
	}
	gz, err := r.get(ctx, rj.IndexV2, 16<<20)
	if err != nil {
		return 0, err
	}
	pb := gz
	if len(gz) > 2 && gz[0] == 0x1f && gz[1] == 0x8b {
		zr, err := gzip.NewReader(strings.NewReader(string(gz)))
		if err != nil {
			return 0, err
		}
		if pb, err = io.ReadAll(io.LimitReader(zr, 64<<20)); err != nil {
			return 0, err
		}
	}
	idx, err := ParseIndex(pb)
	if err != nil {
		return 0, err
	}
	if !strings.EqualFold(idx.SigningKey, r.Fingerprint) {
		return 0, fmt.Errorf("index.pb declara la huella %q, distinta de la fijada", idx.SigningKey)
	}
	// release-assets.json: hash de CONTENIDO por paquete (independiente de versión y firma; no es el sha256 del jar).
	// Se guarda para detectar cambios de contenido; si no se puede bajar, se sigue sin él.
	hashes := map[string]string{}
	if b, err := r.get(ctx, strings.TrimSuffix(r.URL, "repo.json")+"release-assets.json", 8<<20); err == nil {
		json.Unmarshal(b, &hashes)
	}
	tx, err := r.St.DB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	tx.Exec(`DELETE FROM repo_ext`)
	for _, e := range idx.Extensions {
		if e.JarURL == "" {
			continue
		}
		lang := "all"
		if len(e.Sources) > 0 {
			lang = e.Sources[0].Lang
			for _, s := range e.Sources {
				if s.Lang != lang {
					lang = "all"
					break
				}
			}
		}
		sj, _ := json.Marshal(e.Sources)
		if _, err := tx.Exec(`INSERT OR REPLACE INTO repo_ext(pkg,name,lang,version,version_code,nsfw,jar_url,icon_url,sources,content_hash) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			e.Pkg, e.Name, lang, e.VersionName, e.VersionCode, e.ContentWarning, e.JarURL, e.IconURL, string(sj), hashes[e.Pkg]); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	r.St.Set("repo_refreshed", fmt.Sprint(time.Now().Unix()))
	r.St.Set("repo_name", rj.Meta.Name)
	return len(idx.Extensions), nil
}

type SourceInfo struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Lang    string `json:"lang"`
	BaseURL string `json:"baseUrl"`
}

// Uninstall quita una extensión: sus sources, su fila y su jar. Se niega si alguna serie la usa (habría que
// quitarle antes ese source a la serie). El host se reinicia en la siguiente petición sin ella.
func (r *Repo) Uninstall(pkg string) error { return r.uninstall(pkg, false) }

// UninstallTrial quita una extensión instalada solo para probarla (sin anotarlo en la actividad).
func (r *Repo) UninstallTrial(pkg string) error { return r.uninstall(pkg, true) }

func (r *Repo) uninstall(pkg string, quiet bool) error {
	var name, jar string
	if err := r.St.DB.QueryRow(`SELECT name, jar_path FROM ext WHERE pkg=?`, pkg).Scan(&name, &jar); err != nil {
		return fmt.Errorf("%s no está instalada", pkg)
	}
	var n int
	r.St.DB.QueryRow(`SELECT count(*) FROM series_source ss JOIN source s ON s.id=ss.source_id WHERE s.pkg=?`, pkg).Scan(&n)
	if n > 0 {
		return fmt.Errorf("%s la usan %d series: quítasela antes desde su ficha", name, n)
	}
	tx, err := r.St.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var olds []string
	if rows, err := tx.Query(`SELECT jar_path FROM ext_old WHERE pkg=?`, pkg); err == nil {
		for rows.Next() {
			var j string
			rows.Scan(&j)
			olds = append(olds, j)
		}
		rows.Close()
	}
	tx.Exec(`DELETE FROM source WHERE pkg=?`, pkg)
	tx.Exec(`DELETE FROM ext WHERE pkg=?`, pkg)
	tx.Exec(`DELETE FROM ext_old WHERE pkg=?`, pkg)
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, j := range append(olds, jar) {
		if j != "" && filepath.Dir(j) == filepath.Clean(r.Dir) {
			os.Remove(j)
		}
	}
	if !quiet {
		r.St.Event("extensión", "desinstalada %s", name)
	}
	return nil
}

// Install descarga el jar, verifica su firma contra la huella fijada y solo entonces lo carga en el host.
func (r *Repo) Install(ctx context.Context, pkg string) ([]SourceInfo, error) {
	return r.install(ctx, pkg, false)
}

// InstallTrial instala para probar: igual de verificada, pero con sus sources desactivados (no salen en las
// búsquedas ni se usan) y sin anotarlo en la actividad.
func (r *Repo) InstallTrial(ctx context.Context, pkg string) ([]SourceInfo, error) {
	return r.install(ctx, pkg, true)
}

func (r *Repo) install(ctx context.Context, pkg string, trial bool) ([]SourceInfo, error) {
	var name, version, jarURL, chash string
	var vcode int64
	err := r.St.DB.QueryRow(`SELECT name,version,version_code,jar_url,coalesce(content_hash,'') FROM repo_ext WHERE pkg=?`, pkg).
		Scan(&name, &version, &vcode, &jarURL, &chash)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("%s no está en el índice (actualiza el repositorio)", pkg)
	} else if err != nil {
		return nil, err
	}
	u, err := url.Parse(jarURL)
	if err != nil || u.Scheme != "https" || !strings.HasSuffix(u.Path, ".jar") {
		return nil, fmt.Errorf("jarUrl no admitida: %s", jarURL)
	}
	if err := os.MkdirAll(r.Dir, 0o755); err != nil {
		return nil, err
	}
	final := filepath.Join(r.Dir, pkg+"-"+version+".jar")
	tmp := final + ".download"
	defer os.Remove(tmp)
	b, err := r.get(ctx, jarURL, 64<<20)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return nil, err
	}
	// Verificación ANTES de que el jar toque el host.
	if _, err := VerifyJar(tmp, r.Fingerprint); err != nil {
		r.St.Event("seguridad", "jar rechazado %s: %v", pkg, err)
		return nil, fmt.Errorf("verificación de %s: %w", pkg, err)
	}
	sum := sha256.Sum256(b)
	if err := os.Rename(tmp, final); err != nil {
		return nil, err
	}
	res, err := r.Host.CallJar(ctx, final, "load", nil)
	if err != nil {
		os.Remove(final)
		return nil, fmt.Errorf("el host no pudo cargar %s: %w", pkg, err)
	}
	var lr struct {
		Sources []SourceInfo `json:"sources"`
	}
	json.Unmarshal(res, &lr)
	tx, err := r.St.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var oldJar, oldVer, oldSum, oldHash string
	var oldCode int64
	tx.QueryRow(`SELECT jar_path, version, version_code, jar_sha256, coalesce(content_hash,'') FROM ext WHERE pkg=?`, pkg).
		Scan(&oldJar, &oldVer, &oldCode, &oldSum, &oldHash)
	if oldJar != "" && oldJar != final { // la versión que se sustituye se guarda por si hay que volver a ella
		tx.Exec(`INSERT OR REPLACE INTO ext_old(pkg,version,version_code,jar_path,jar_sha256,content_hash,kept_at) VALUES(?,?,?,?,?,?,?)`,
			pkg, oldVer, oldCode, oldJar, oldSum, oldHash, time.Now().Unix())
	}
	tx.Exec(`DELETE FROM ext_old WHERE pkg=? AND version=?`, pkg, version)
	tx.Exec(`INSERT INTO ext(pkg,name,version,version_code,jar_path,jar_sha256,content_hash,installed_at) VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(pkg) DO UPDATE SET name=excluded.name,version=excluded.version,version_code=excluded.version_code,
		jar_path=excluded.jar_path,jar_sha256=excluded.jar_sha256,content_hash=excluded.content_hash,installed_at=excluded.installed_at`,
		pkg, name, version, vcode, final, hex.EncodeToString(sum[:]), chash, time.Now().Unix())
	for _, s := range lr.Sources {
		tx.Exec(`INSERT INTO source(id,pkg,name,lang,base_url,enabled) VALUES(?,?,?,?,?,?)
			ON CONFLICT(id) DO UPDATE SET pkg=excluded.pkg,name=excluded.name,lang=excluded.lang,base_url=excluded.base_url`,
			s.ID, pkg, s.Name, s.Lang, s.BaseURL, b2i(!trial))
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	r.pruneOld(pkg)
	if !trial {
		r.St.Event("extensión", "instalada %s %s (%d sources, firma verificada)", name, version, len(lr.Sources))
	}
	return lr.Sources, nil
}

// keepOld: cuántas versiones anteriores de cada extensión se guardan.
const keepOld = 2

// pruneOld borra las versiones guardadas que sobran (las más antiguas) y sus jars.
func (r *Repo) pruneOld(pkg string) {
	rows, err := r.St.DB.Query(`SELECT version, jar_path FROM ext_old WHERE pkg=? ORDER BY kept_at DESC`, pkg)
	if err != nil {
		return
	}
	type old struct{ ver, jar string }
	var drop []old
	for i := 0; rows.Next(); i++ {
		var o old
		rows.Scan(&o.ver, &o.jar)
		if i >= keepOld {
			drop = append(drop, o)
		}
	}
	rows.Close()
	for _, o := range drop {
		r.St.DB.Exec(`DELETE FROM ext_old WHERE pkg=? AND version=?`, pkg, o.ver)
		if filepath.Dir(o.jar) == filepath.Clean(r.Dir) {
			os.Remove(o.jar)
		}
	}
}

// OldVersions: las versiones anteriores guardadas de una extensión, de la más reciente a la más antigua.
func (r *Repo) OldVersions(pkg string) []string {
	var vs []string
	rows, err := r.St.DB.Query(`SELECT version FROM ext_old WHERE pkg=? ORDER BY kept_at DESC`, pkg)
	if err != nil {
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		var v string
		rows.Scan(&v)
		vs = append(vs, v)
	}
	return vs
}

// SetPinned: con pinned, «Actualizar todas» se salta esta extensión.
func (r *Repo) SetPinned(pkg string, pinned bool) {
	r.St.DB.Exec(`UPDATE ext SET pinned=? WHERE pkg=?`, b2i(pinned), pkg)
}

// UseVersion vuelve a una versión guardada: comprueba que su jar no ha cambiado, la carga en el host y la deja
// como la instalada (la de ahora pasa a guardadas). Queda fijada para que no se vuelva a actualizar sola.
func (r *Repo) UseVersion(ctx context.Context, pkg, version string) error {
	var jar, sum, chash string
	var code int64
	if err := r.St.DB.QueryRow(`SELECT jar_path, jar_sha256, coalesce(content_hash,''), version_code FROM ext_old WHERE pkg=? AND version=?`,
		pkg, version).Scan(&jar, &sum, &chash, &code); err != nil {
		return fmt.Errorf("la versión %s de %s no está guardada", version, pkg)
	}
	f, err := os.Open(jar)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(h, f)
	f.Close()
	if err != nil || hex.EncodeToString(h.Sum(nil)) != sum {
		return fmt.Errorf("el jar de la versión %s ha cambiado en disco: no se usa", version)
	}
	res, err := r.Host.CallJar(ctx, jar, "load", nil)
	if err != nil {
		return fmt.Errorf("el host no pudo cargar la versión %s: %w", version, err)
	}
	var lr struct {
		Sources []SourceInfo `json:"sources"`
	}
	json.Unmarshal(res, &lr)
	tx, err := r.St.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var name, curJar, curVer, curSum, curHash string
	var curCode int64
	if err := tx.QueryRow(`SELECT name, jar_path, version, version_code, jar_sha256, coalesce(content_hash,'') FROM ext WHERE pkg=?`, pkg).
		Scan(&name, &curJar, &curVer, &curCode, &curSum, &curHash); err != nil {
		return fmt.Errorf("%s no está instalada", pkg)
	}
	tx.Exec(`INSERT OR REPLACE INTO ext_old(pkg,version,version_code,jar_path,jar_sha256,content_hash,kept_at) VALUES(?,?,?,?,?,?,?)`,
		pkg, curVer, curCode, curJar, curSum, curHash, time.Now().Unix())
	tx.Exec(`DELETE FROM ext_old WHERE pkg=? AND version=?`, pkg, version)
	tx.Exec(`UPDATE ext SET version=?, version_code=?, jar_path=?, jar_sha256=?, content_hash=?, pinned=1 WHERE pkg=?`,
		version, code, jar, sum, chash, pkg)
	for _, s := range lr.Sources { // por si la otra versión cambia nombres o dominios (los ids no cambian)
		tx.Exec(`UPDATE source SET name=?, lang=?, base_url=? WHERE id=? AND pkg=?`, s.Name, s.Lang, s.BaseURL, s.ID, pkg)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	verified.Delete(curJar)
	r.St.Event("extensión", "%s vuelve a la versión %s (antes %s); fijada para que no se actualice sola", name, version, curVer)
	return nil
}

// UpdateInstalled refresca el índice y reinstala (con verificación de firma) las extensiones instaladas cuya versión
// del repositorio es distinta. Devuelve cuántas actualizó.
func (r *Repo) UpdateInstalled(ctx context.Context) (int, error) {
	if _, err := r.Refresh(ctx); err != nil {
		return 0, err
	}
	rows, err := r.St.DB.Query(`SELECT e.pkg FROM ext e JOIN repo_ext x ON x.pkg=e.pkg WHERE e.pinned=0 AND (x.version_code<>e.version_code OR x.version<>e.version)`)
	if err != nil {
		return 0, err
	}
	var pkgs []string
	for rows.Next() {
		var p string
		rows.Scan(&p)
		pkgs = append(pkgs, p)
	}
	rows.Close()
	n := 0
	var firstErr error
	for _, p := range pkgs {
		if _, err := r.Install(ctx, p); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		n++
	}
	return n, firstErr
}

// JarFor devuelve el jar instalado de un source, volviendo a verificar su sha256 guardado
// (un jar modificado en disco después de instalarlo no se carga).
func (r *Repo) JarFor(sourceID string) (jar string, err error) {
	var sum string
	err = r.St.DB.QueryRow(`SELECT e.jar_path, e.jar_sha256 FROM source s JOIN ext e ON e.pkg=s.pkg WHERE s.id=?`, sourceID).Scan(&jar, &sum)
	if err != nil {
		return "", fmt.Errorf("source %s sin extensión instalada", sourceID)
	}
	if v, ok := verified.Load(jar); ok && v.(string) == sum {
		return jar, nil
	}
	f, err := os.Open(jar)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	if hex.EncodeToString(h.Sum(nil)) != sum {
		return "", fmt.Errorf("el jar %s ha cambiado en disco desde su instalación: no se carga", filepath.Base(jar))
	}
	verified.Store(jar, sum)
	return jar, nil
}

var verified syncMap

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
