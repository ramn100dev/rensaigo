// Package web contiene la UI embebida (plantillas renderizadas en servidor + htmx; sin Node ni build).
package web

import "embed"

//go:embed templates/*.html static/*
var FS embed.FS
