package api

import (
	"encoding/json"
	"html/template"

	"rensaigo/internal/i18n"
)

// jsTexts: los textos que pone el JavaScript (app.js, reader.js). Van a window.I18N ya traducidos.
var jsTexts = []string{
	"Sin conexión con el servidor", "Cargando…", "Añadir serie",
	"¿Eliminar «%s»?\n\nSe BORRA también su carpeta con todos los CBZ. No se puede deshacer.",
	"Serie añadida", "%s series añadidas", "Añadiendo…", "Añadir", "Buscando %s…", "%s sin source elegido",
	"Página", "Vertical", "Paginado", "Manga (←)", "Occidental (→)",
}

func jsDict() template.JS {
	m := map[string]string{}
	if i18n.Lang() != "es" {
		for _, s := range jsTexts {
			m[s] = i18n.T(s)
		}
	}
	b, _ := json.Marshal(m)
	return template.JS(b)
}

// optLabel: cómo se ve una opción de un ajuste (el valor que se guarda no cambia).
func optLabel(v string) string {
	switch v {
	case "en":
		return "English"
	case "es":
		return "Español"
	}
	return i18n.T(v)
}
