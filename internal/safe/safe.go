// Package safe: un fallo inesperado (panic) en una tarea de fondo se anota en el log en vez de tumbar el proceso
// entero. Las peticiones HTTP ya las protege net/http; esto es para las gorutinas propias.
package safe

import (
	"context"
	"fmt"
	"log"
	"runtime/debug"
	"time"
)

// Recover se usa con defer al principio de una gorutina: si entra en pánico, se registra y la gorutina termina.
func Recover(what string) {
	if r := recover(); r != nil {
		log.Printf("%s: fallo inesperado (panic): %v\n%s", what, r, debug.Stack())
	}
}

// Go lanza f en una gorutina protegida.
func Go(what string, f func()) {
	go func() {
		defer Recover(what)
		f()
	}()
}

// Call ejecuta f y convierte un pánico en error (para quien tiene que seguir y anotar el fallo).
func Call(what string, f func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("%s: fallo inesperado (panic): %v\n%s", what, r, debug.Stack())
			err = fmt.Errorf("%s: fallo interno: %v", what, r)
		}
	}()
	return f()
}

// Loop ejecuta un bucle de fondo que debería durar hasta que se cancele ctx. Si entra en pánico, se registra y se
// vuelve a arrancar al cabo de un rato.
func Loop(ctx context.Context, what string, f func(context.Context)) {
	for ctx.Err() == nil {
		func() {
			defer Recover(what)
			f(ctx)
		}()
		select {
		case <-ctx.Done():
		case <-time.After(30 * time.Second):
		}
	}
}
