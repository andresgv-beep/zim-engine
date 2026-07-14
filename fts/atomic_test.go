package fts

import (
	"os"
	"path/filepath"
	"testing"
)

// Tests de la publicación atómica (INDEXER-CRASH-SAFETY.md, Capas 1-2). Simulan
// los estados en disco que deja un corte en cada punto del build/swap y verifican
// que Reconcile converge al estado correcto. Operan a nivel de directorio (un
// índice bleve real no hace falta: la lógica va por presencia de manifiesto).

// makeDir crea un directorio "índice" con un marcador de identidad y, opcional,
// el manifiesto (su sello de completo).
func makeDir(t *testing.T, dir string, withManifest bool, who string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "who.txt"), []byte(who), 0o644); err != nil {
		t.Fatal(err)
	}
	if withManifest {
		if err := os.WriteFile(filepath.Join(dir, ManifestName), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func who(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "who.txt"))
	if err != nil {
		return "<ausente>"
	}
	return string(b)
}

// promote reemplaza el índice vivo por el nuevo, y no deja trash ni building.
func TestPromoteReplacesLiveIndex(t *testing.T) {
	base := filepath.Join(t.TempDir(), "col.bleve")
	makeDir(t, base, true, "viejo")
	makeDir(t, buildingDir(base), true, "nuevo")

	if err := promote(buildingDir(base), base); err != nil {
		t.Fatal(err)
	}
	if got := who(t, base); got != "nuevo" {
		t.Fatalf("el índice vivo debería ser el nuevo, es %q", got)
	}
	if dirExists(buildingDir(base)) || dirExists(trashDir(base)) {
		t.Fatal("promote dejó .new o .old colgando")
	}
}

// Corte tras escribir el manifiesto del build, antes del swap: Reconcile lo promueve.
func TestReconcilePromotesCompletedBuild(t *testing.T) {
	base := filepath.Join(t.TempDir(), "col.bleve")
	makeDir(t, buildingDir(base), true, "nuevo") // build entero, no cambiado
	// no hay final: colección nueva

	if err := Reconcile(base); err != nil {
		t.Fatal(err)
	}
	if who(t, base) != "nuevo" {
		t.Fatal("un build completo sin cambiar debería promoverse a índice vivo")
	}
	if dirExists(buildingDir(base)) {
		t.Fatal(".new debería haber desaparecido tras promover")
	}
}

// Corte a mitad del build (sin manifiesto): Reconcile lo tira y respeta el vivo.
func TestReconcileDropsIncompleteBuild(t *testing.T) {
	base := filepath.Join(t.TempDir(), "col.bleve")
	makeDir(t, base, true, "bueno")                // índice vivo intacto
	makeDir(t, buildingDir(base), false, "medias") // build interrumpido

	if err := Reconcile(base); err != nil {
		t.Fatal(err)
	}
	if dirExists(buildingDir(base)) {
		t.Fatal("un build a medias debería borrarse")
	}
	if who(t, base) != "bueno" {
		t.Fatal("el índice vivo NO debía tocarse")
	}
}

// Corte del swap tras mover el viejo a .old, antes de poner el nuevo: se restaura.
func TestReconcileRestoresFromTrash(t *testing.T) {
	base := filepath.Join(t.TempDir(), "col.bleve")
	makeDir(t, trashDir(base), true, "viejo") // final movido a .old, luego apagón
	// no hay final

	if err := Reconcile(base); err != nil {
		t.Fatal(err)
	}
	if who(t, base) != "viejo" {
		t.Fatal("con final ausente y .old bueno, .old debe restaurarse")
	}
	if dirExists(trashDir(base)) {
		t.Fatal(".old debería haberse consumido")
	}
}

// Corte del swap tras poner el nuevo, antes de borrar .old: se limpia el .old.
func TestReconcileDropsTrashWhenFinalGood(t *testing.T) {
	base := filepath.Join(t.TempDir(), "col.bleve")
	makeDir(t, base, true, "nuevo")           // swap ya puso el nuevo
	makeDir(t, trashDir(base), true, "viejo") // .old pendiente de borrar

	if err := Reconcile(base); err != nil {
		t.Fatal(err)
	}
	if who(t, base) != "nuevo" {
		t.Fatal("el final bueno no debía cambiar")
	}
	if dirExists(trashDir(base)) {
		t.Fatal(".old debería borrarse cuando el final ya es bueno")
	}
}

// Un final sin sello (build viejo interrumpido o pre-Capa-1) se descarta.
func TestReconcileDropsFinalWithoutManifest(t *testing.T) {
	base := filepath.Join(t.TempDir(), "col.bleve")
	makeDir(t, base, false, "roto")

	if err := Reconcile(base); err != nil {
		t.Fatal(err)
	}
	if dirExists(base) {
		t.Fatal("un final sin manifiesto debería borrarse para reconstruirse")
	}
}

// Estado sano: Reconcile no toca nada.
func TestReconcileNoopOnHealthy(t *testing.T) {
	base := filepath.Join(t.TempDir(), "col.bleve")
	makeDir(t, base, true, "bueno")

	if err := Reconcile(base); err != nil {
		t.Fatal(err)
	}
	if who(t, base) != "bueno" {
		t.Fatal("Reconcile no debía alterar un índice sano")
	}
}
