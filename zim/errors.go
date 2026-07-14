// Package zim es un lector NATIVO de archivos ZIM en Go puro (cero cgo, cero deps
// de runtime externas). Lee los .zim directamente del disco con os.File + ReadAt;
// NO depende de kiwix-serve ni de Docker. Es un paquete genérico: no conoce HTTP,
// ni usuarios, ni el shim de Library (misma filosofía que download/).
//
// Estado: FASE A · paso 1 (cimientos). Aquí viven los errores tipados, los límites
// defensivos, la API pública y el esqueleto de ciclo de vida. El parser (header,
// dirents, clusters) llega en el paso 2. Ver ZIM-ENGINE.md.
package zim

import "errors"

// Errores tipados: hay más de dos estados de fallo, así que nada de (valor, bool).
// El handler HTTP los traduce a códigos (404/422/503/500); zim/ no conoce HTTP.
var (
	ErrNotFound               = errors.New("zim: entry not found")
	ErrCorrupt                = errors.New("zim: corrupt archive")
	ErrUnsupportedVersion     = errors.New("zim: unsupported version")
	ErrUnsupportedCompression = errors.New("zim: unsupported compression")
	ErrRedirectCycle          = errors.New("zim: redirect cycle")
	ErrResourceLimit          = errors.New("zim: resource limit exceeded")
	ErrClosed                 = errors.New("zim: archive closed")
)
