// Package i18n: idioma de la interfaz. Los textos se escriben en español en el código y en las plantillas, y ese
// texto es la clave del catálogo inglés (en_*.go). Un texto sin traducción sale en español; el test lo detecta.
package i18n

import (
	"errors"
	"fmt"
	"sync/atomic"
)

var cur atomic.Value // "en" | "es"

func init() { cur.Store("en") }

// Langs: idiomas de la interfaz.
var Langs = []string{"en", "es"}

// Set cambia el idioma de la interfaz (cualquier cosa que no sea "es" es inglés).
func Set(l string) {
	if l != "es" {
		l = "en"
	}
	cur.Store(l)
}

func Lang() string { return cur.Load().(string) }

var en = map[string]string{}

func add(m map[string]string) {
	for k, v := range m {
		en[k] = v
	}
}

// T traduce un texto.
func T(s string) string {
	if Lang() == "es" {
		return s
	}
	if v, ok := en[s]; ok {
		return v
	}
	return s
}

// Tf traduce el formato y le aplica los argumentos.
func Tf(format string, a ...any) string { return fmt.Sprintf(T(format), a...) }

// Tn: singular o plural según n; los dos llevan %d.
func Tn(n any, one, many string) string {
	f := many
	switch v := n.(type) {
	case int:
		if v == 1 {
			f = one
		}
	case int64:
		if v == 1 {
			f = one
		}
	case float64:
		if v == 1 {
			f = one
		}
	}
	return fmt.Sprintf(T(f), n)
}

// Errorf: un error con el texto traducido.
func Errorf(format string, a ...any) error {
	if len(a) == 0 {
		return errors.New(T(format))
	}
	return fmt.Errorf(T(format), a...)
}

// Has dice si hay traducción (para los tests).
func Has(s string) bool { _, ok := en[s]; return ok }
