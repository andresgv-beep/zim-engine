package fts

// Publicación atómica del índice (INDEXER-CRASH-SAFETY.md, Capas 1-2).
//
// Regla de oro: NUNCA se escribe sobre el índice vivo. Build construye en un
// directorio aparte (<dir>.new) y solo cuando está entero —con su manifiesto—
// lo cambia por el bueno de golpe. Un apagón a mitad deja el .new a medias
// (basura reconocible) y el índice vivo INTACTO. Reconcile limpia los restos de
// un corte y promueve un build completo que no llegó a cambiarse.
//
// Invariante que lo gobierna todo: EL ÍNDICE BUENO ES EL QUE TIENE MANIFIESTO.
// El manifiesto (zim-fts.json) se escribe lo último, tras el Close del Builder;
// su presencia certifica build completo. Sin él, es un build interrumpido.

import (
	"os"
	"path/filepath"
)

// buildingDir: destino de construcción. Sufijo distinto del final para que un
// build a medias jamás se confunda con un índice bueno.
func buildingDir(final string) string { return final + ".new" }

// trashDir: cajón temporal del índice viejo durante el cambio. Solo existe
// dentro de la ventana de milisegundos del swap.
func trashDir(final string) string { return final + ".old" }

// hasManifest: true si el directorio contiene un índice COMPLETO (su sello).
func hasManifest(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ManifestName))
	return err == nil
}

func dirExists(dir string) bool {
	fi, err := os.Stat(dir)
	return err == nil && fi.IsDir()
}

// promote cambia el índice recién construido (building) por el bueno (final) de
// forma resistente a un corte en CUALQUIER punto:
//
//	P0. borrar restos de trash de un intento anterior
//	P1. si final existe → moverlo a trash
//	P2. mover building → final
//	P3. borrar trash
//
// Si el proceso muere entre P1 y P2, Reconcile ve building (con manifiesto) y lo
// vuelve a promover; entre P2 y P3, ve final bueno + trash y borra trash. Es
// idempotente: repetir promote/Reconcile converge al mismo estado.
//
// PRECONDICIÓN (Windows): final NO puede tener ficheros abiertos —un índice en
// uso por el shim bloquea el rename—. La orquestación del Panel cerrará el índice
// (invalidate) antes de reconstruir; por CLI no hay nadie que lo tenga abierto.
// En POSIX el rename sobre un dir abierto funciona (el inodo sobrevive al fd).
func promote(building, final string) error {
	trash := trashDir(final)
	if err := os.RemoveAll(trash); err != nil {
		return err
	}
	if dirExists(final) {
		if err := os.Rename(final, trash); err != nil {
			return err
		}
	}
	if err := os.Rename(building, final); err != nil {
		return err
	}
	return os.RemoveAll(trash)
}

// Reconcile deja el índice de una colección en un estado coherente tras un
// arranque sucio (apagón/kill a mitad de un build o de un swap). Se llama al
// abrir (fts.Open) y al empezar un Build. Que no haya nada que reconciliar es el
// caso normal, no un error.
//
// Reglas, en orden (cada una asume la anterior aplicada):
//  1. Un build COMPLETO sin cambiar (building con manifiesto) siempre gana: se promueve.
//  2. Un building a medias (sin manifiesto) es basura de un corte: se borra.
//  3. Un trash colgando de un swap interrumpido: si final es bueno, se borra; si
//     final falta o está roto pero trash es bueno, se promueve trash.
//  4. Un final sin manifiesto es un build interrumpido (o pre-Capa-1): se borra
//     para que se reconstruya. Nunca se sirve un índice sin sello.
//
// No es seguro para llamadas concurrentes sobre el MISMO dir (hace renames): el
// shim lo llama una vez por colección al abrir, y el CLI es de un solo hilo.
func Reconcile(final string) error {
	building := buildingDir(final)
	trash := trashDir(final)

	if dirExists(building) {
		if hasManifest(building) {
			if err := promote(building, final); err != nil {
				return err
			}
		} else if err := os.RemoveAll(building); err != nil {
			return err
		}
	}

	if dirExists(trash) {
		switch {
		case dirExists(final) && hasManifest(final):
			if err := os.RemoveAll(trash); err != nil {
				return err
			}
		case hasManifest(trash):
			if err := os.RemoveAll(final); err != nil { // final ausente o roto: dejar sitio
				return err
			}
			if err := os.Rename(trash, final); err != nil {
				return err
			}
		default:
			if err := os.RemoveAll(trash); err != nil {
				return err
			}
		}
	}

	if dirExists(final) && !hasManifest(final) {
		if err := os.RemoveAll(final); err != nil {
			return err
		}
	}
	return nil
}
