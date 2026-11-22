package extrepo

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"rensaigo/internal/i18n"
	"strings"

	"github.com/smallstep/pkcs7"
)

// VerifyJar comprueba la firma JAR (esquema v1) de una extensión ANTES de cargarla:
//  1. META-INF/*.RSA es un PKCS#7 válido sobre el fichero .SF;
//  2. la huella SHA-256 del certificado firmante es la huella fijada del repositorio;
//  3. el .SF fija el digest del MANIFEST.MF;
//  4. cada fichero del jar aparece en el manifiesto con su SHA-256 correcto, sin entradas extra ni repetidas.
//
// Devuelve la huella del firmante.
func VerifyJar(path, wantFingerprint string) (string, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return "", err
	}
	defer zr.Close()
	files := map[string]*zip.File{}
	var sfName, sigName string
	for _, f := range zr.File {
		if _, dup := files[f.Name]; dup {
			return "", i18n.Errorf("entrada repetida en el jar: %s", f.Name)
		}
		files[f.Name] = f
		up := strings.ToUpper(f.Name)
		if strings.HasPrefix(up, "META-INF/") && !strings.Contains(f.Name[9:], "/") {
			switch {
			case strings.HasSuffix(up, ".SF"):
				if sfName != "" {
					return "", i18n.Errorf("más de un fichero .SF")
				}
				sfName = f.Name
			case strings.HasSuffix(up, ".RSA"), strings.HasSuffix(up, ".EC"), strings.HasSuffix(up, ".DSA"):
				if sigName != "" {
					return "", i18n.Errorf("más de un bloque de firma")
				}
				sigName = f.Name
			}
		}
	}
	if sfName == "" || sigName == "" || files["META-INF/MANIFEST.MF"] == nil {
		return "", i18n.Errorf("el jar no está firmado (faltan MANIFEST.MF, .SF o el bloque de firma)")
	}
	read := func(name string) ([]byte, error) {
		rc, err := files[name].Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(io.LimitReader(rc, 8<<20))
	}
	sig, err := read(sigName)
	if err != nil {
		return "", err
	}
	sf, err := read(sfName)
	if err != nil {
		return "", err
	}
	mf, err := read("META-INF/MANIFEST.MF")
	if err != nil {
		return "", err
	}
	p7, err := pkcs7.Parse(sig)
	if err != nil {
		return "", i18n.Errorf("bloque de firma ilegible: %w", err)
	}
	p7.Content = sf
	if err := p7.Verify(); err != nil {
		return "", i18n.Errorf("la firma no valida el .SF: %w", err)
	}
	signer := p7.GetOnlySigner()
	if signer == nil {
		return "", i18n.Errorf("se esperaba exactamente un firmante")
	}
	sum := sha256.Sum256(signer.Raw)
	fp := hex.EncodeToString(sum[:])
	if !strings.EqualFold(fp, wantFingerprint) {
		return fp, i18n.Errorf("firmante inesperado: %s (se esperaba %s)", fp, wantFingerprint)
	}
	// .SF -> manifiesto completo
	sfMain, _ := parseManifest(sf)
	want := sfMain["SHA-256-Digest-Manifest"]
	mfSum := sha256.Sum256(mf)
	if want == "" || want != base64.StdEncoding.EncodeToString(mfSum[:]) {
		return fp, i18n.Errorf("el digest del manifiesto no coincide con el .SF")
	}
	// manifiesto -> cada fichero
	_, entries := parseManifest(mf)
	for name, f := range files {
		if f.FileInfo().IsDir() || name == "META-INF/MANIFEST.MF" || name == sfName || name == sigName {
			continue
		}
		digest, ok := entries[name]
		if !ok {
			return fp, i18n.Errorf("fichero sin firmar en el jar: %s", name)
		}
		rc, err := f.Open()
		if err != nil {
			return fp, err
		}
		h := sha256.New()
		_, err = io.Copy(h, rc)
		rc.Close()
		if err != nil {
			return fp, err
		}
		if base64.StdEncoding.EncodeToString(h.Sum(nil)) != digest {
			return fp, fmt.Errorf("digest incorrecto: %s", name)
		}
	}
	return fp, nil
}

// parseManifest devuelve los atributos principales y, por entrada, su SHA-256-Digest.
// Formato JAR: secciones separadas por línea en blanco; las líneas largas continúan con un espacio inicial.
func parseManifest(b []byte) (main map[string]string, entries map[string]string) {
	main, entries = map[string]string{}, map[string]string{}
	text := strings.ReplaceAll(string(bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))), "\n ", "")
	for i, sec := range strings.Split(text, "\n\n") {
		attrs := map[string]string{}
		for _, l := range strings.Split(sec, "\n") {
			if k, v, ok := strings.Cut(l, ": "); ok {
				attrs[k] = v
			}
		}
		if i == 0 {
			main = attrs
			if _, isEntry := attrs["Name"]; !isEntry {
				continue
			}
		}
		if n := attrs["Name"]; n != "" {
			entries[n] = attrs["SHA-256-Digest"]
		}
	}
	return
}
