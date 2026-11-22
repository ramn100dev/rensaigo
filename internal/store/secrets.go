package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// Ajustes con secretos (tokens de MAL/AniList/MangaDex, canales de notificación, clave de OPDS…): se guardan
// cifrados con AES-256-GCM. La clave vive aparte, en <config>/secret.key (0600), así que una copia de la base de
// datos sola no deja ver nada. Get y Set cifran y descifran solos: el resto del código no se entera.

const encPrefix = "enc1:"

// isSecret: qué ajustes van cifrados.
func isSecret(key string) bool {
	switch key {
	case "auth.token", "notify.channels", "opds.key":
		return true
	}
	for _, suf := range []string{".access", ".refresh", ".client_secret", ".verifier", ".password"} {
		if strings.HasSuffix(key, suf) {
			return true
		}
	}
	return false
}

// EnableSecrets carga (o crea la primera vez) la clave y cifra los secretos que aún estén en claro.
func (s *Store) EnableSecrets(keyPath string) error {
	key, err := os.ReadFile(keyPath)
	if errors.Is(err, os.ErrNotExist) {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(keyPath), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(keyPath+".tmp", key, 0o600); err != nil {
			return err
		}
		if err := os.Rename(keyPath+".tmp", keyPath); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if len(key) != 32 {
		return fmt.Errorf("%s: la clave debe tener 32 bytes (tiene %d)", keyPath, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	if s.aead, err = cipher.NewGCM(block); err != nil {
		return err
	}
	// los que se guardaron antes de esto (o con la clave quitada) se cifran ahora
	rows, err := s.DB.Query(`SELECT key, value FROM setting`)
	if err != nil {
		return err
	}
	plain := map[string]string{}
	for rows.Next() {
		var k, v string
		rows.Scan(&k, &v)
		if isSecret(k) && v != "" && !strings.HasPrefix(v, encPrefix) {
			plain[k] = v
		}
	}
	rows.Close()
	for k, v := range plain {
		s.Set(k, v)
	}
	if len(plain) > 0 {
		log.Printf("secretos: cifrados %d ajustes que estaban en claro", len(plain))
	}
	return nil
}

func (s *Store) seal(v string) string {
	nonce := make([]byte, s.aead.NonceSize())
	rand.Read(nonce)
	return encPrefix + base64.RawStdEncoding.EncodeToString(s.aead.Seal(nonce, nonce, []byte(v), nil))
}

func (s *Store) open(v string) (string, error) {
	if s.aead == nil {
		return "", errors.New("no hay clave")
	}
	b, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(v, encPrefix))
	if err != nil || len(b) < s.aead.NonceSize() {
		return "", errors.New("valor cifrado dañado")
	}
	n := s.aead.NonceSize()
	out, err := s.aead.Open(nil, b[:n], b[n:], nil)
	if err != nil {
		return "", errors.New("no se puede descifrar (¿cambió secret.key?)")
	}
	return string(out), nil
}
