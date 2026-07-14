package fts

// Tests que anclan los tres arreglos de FTS-AUDIT.md. Si alguien deshace uno,
// esto se pone rojo con el número del bug delante.

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/blevesearch/bleve/v2"
)

// buildTinyIndex crea un índice bleve mínimo válido en dir (sin pasar por Build,
// que necesita un Archive real) y le escribe el manifiesto que se le pida.
func buildTinyIndex(t *testing.T, dir string, m *Manifest) {
	t.Helper()
	idx, err := bleve.New(dir, bleve.NewIndexMapping())
	if err != nil {
		t.Fatalf("bleve.New: %v", err)
	}
	if err := idx.Index("C/Doc", map[string]string{"title": "Doc", "body": "hola mundo"}); err != nil {
		t.Fatalf("index: %v", err)
	}
	if err := idx.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if m != nil {
		if err := writeManifest(dir, *m); err != nil {
			t.Fatalf("writeManifest: %v", err)
		}
	}
}

func manifestFor(uuid byte, count uint32) Manifest {
	var u [16]byte
	u[0] = uuid
	a := fakeArchive{uuid: u, count: count}
	return newManifest(a, BuildOptions{Language: "es"}, Tally{Candidates: 1, Indexed: 1})
}

// ── BUG-1: Open verifica el manifiesto ─────────────────────────────────────

// El agujero original: copiar el .bleve de OTRO ZIM servía resultados fantasma.
func TestOpenRejectsForeignIndex(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "idx")
	m := manifestFor(0xAA, 100) // índice construido para el ZIM AA
	buildTinyIndex(t, dir, &m)

	var otherUUID [16]byte
	otherUUID[0] = 0xBB // …y se intenta abrir contra el ZIM BB
	other := fakeArchive{uuid: otherUUID, count: 100}

	if _, err := Open(dir, other); err == nil {
		t.Fatal("BUG-1 reabierto: Open aceptó un índice de OTRO ZIM")
	} else if !strings.Contains(err.Error(), "otro ZIM") {
		t.Fatalf("quería el error de uuid, tengo: %v", err)
	}
}

// Sin manifiesto = build a medias (o pre-manifiesto): tratar como inexistente.
func TestOpenRejectsMissingManifest(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "idx")
	buildTinyIndex(t, dir, nil) // índice válido, PERO sin zim-fts.json

	var u [16]byte
	u[0] = 0xAA
	if _, err := Open(dir, fakeArchive{uuid: u, count: 100}); err == nil {
		t.Fatal("BUG-1 reabierto: Open aceptó un índice SIN manifiesto")
	}
}

// Esquema viejo → este binario no sabe si lo lee bien → reindexar.
func TestOpenRejectsOldSchema(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "idx")
	m := manifestFor(0xAA, 100)
	m.Schema = SchemaVersion - 1
	buildTinyIndex(t, dir, &m)

	var u [16]byte
	u[0] = 0xAA
	if _, err := Open(dir, fakeArchive{uuid: u, count: 100}); err == nil {
		t.Fatal("BUG-1 reabierto: Open aceptó un esquema viejo")
	}
}

// entryCount distinto = el ZIM cambió bajo el índice (o es otra edición).
func TestOpenRejectsEntryCountMismatch(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "idx")
	m := manifestFor(0xAA, 100)
	buildTinyIndex(t, dir, &m)

	var u [16]byte
	u[0] = 0xAA
	if _, err := Open(dir, fakeArchive{uuid: u, count: 999}); err == nil {
		t.Fatal("BUG-1 reabierto: Open aceptó un entryCount distinto")
	}
}

// Y el camino feliz: el índice CORRECTO abre, busca y expone su manifiesto.
func TestOpenAcceptsMatchingIndex(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "idx")
	m := manifestFor(0xAA, 100)
	buildTinyIndex(t, dir, &m)

	var u [16]byte
	u[0] = 0xAA
	idx, err := Open(dir, fakeArchive{uuid: u, count: 100})
	if err != nil {
		t.Fatalf("Open del índice correcto: %v", err)
	}
	defer idx.Close()
	if idx.Manifest().ZimUUID != m.ZimUUID {
		t.Fatal("el Manifest() expuesto no es el verificado")
	}
	if hits, _, err := idx.Search("hola", 5); err != nil || len(hits) == 0 {
		t.Fatalf("search tras Open verificado: hits=%d err=%v", len(hits), err)
	}
}

// ── BUG-2: completitud honesta en el manifiesto ────────────────────────────

func TestManifestCarriesTally(t *testing.T) {
	var u [16]byte
	u[0] = 0xCC
	a := fakeArchive{uuid: u, count: 500}
	m := newManifest(a, BuildOptions{Language: "es"}, Tally{
		Candidates: 400, Indexed: 390, Skipped: 8, Failed: 2,
	})
	if m.Candidates != 400 || m.Indexed != 390 || m.Skipped != 8 || m.Failed != 2 {
		t.Fatalf("BUG-2 reabierto: el tally no viaja en el manifiesto: %+v", m)
	}
	// Y sobrevive el round-trip a disco.
	dir := t.TempDir()
	if err := writeManifest(dir, m); err != nil {
		t.Fatal(err)
	}
	back, err := ReadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if back != m {
		t.Fatalf("round-trip alteró el manifiesto:\n  antes %+v\n  después %+v", m, back)
	}
}

// ── BUG-3: el título viene SIEMPRE del dirent, nunca del HTML ──────────────

// extractText devuelve solo el cuerpo; el <title> del HTML se ignora a propósito
// (una sola fuente de verdad: e.Title(), la misma que usa el suggest).
func TestExtractIgnoresHTMLTitle(t *testing.T) {
	h := `<html><head><title>Saturno - Wikipedia</title></head>` +
		`<body><p>El planeta de los anillos.</p></body></html>`
	body := extractText(strings.NewReader(h))
	if body != "El planeta de los anillos." {
		t.Fatalf("body inesperado: %q", body)
	}
	if strings.Contains(body, "Wikipedia") {
		t.Fatal("BUG-3 reabierto: el <title> se coló en el cuerpo")
	}
}

// ── extract.go: la matriz mínima que faltaba ───────────────────────────────

func TestExtractBlocksDoNotGlue(t *testing.T) {
	body := extractText(strings.NewReader(`<p>foo</p><p>bar</p>`))
	if body != "foo bar" {
		t.Fatalf("bloques pegados: %q (quería \"foo bar\")", body)
	}
}

func TestExtractSkipsScriptStyleAndNavboxes(t *testing.T) {
	h := `<body><script>evil()</script><style>.x{}</style>` +
		`<div class="navbox">chrome de navegación</div>` +
		`<div id="catlinks">categorías</div>` +
		`<p>texto real</p></body>`
	body := extractText(strings.NewReader(h))
	if body != "texto real" {
		t.Fatalf("el filtro dejó pasar basura: %q", body)
	}
}

func TestExtractBrokenHTMLNeverPanics(t *testing.T) {
	cases := []string{
		"",
		"<p>sin cerrar",
		"<html><body><div><div><div>",
		"\x00\x01\x02 binario que no es html \xff",
		strings.Repeat("<a href='x'>", 5000),
	}
	for _, c := range cases {
		_ = extractText(strings.NewReader(c)) // no panic = pasa
	}
}

func TestExtractRespectsSizeLimit(t *testing.T) {
	// > maxArticleBytes: se trunca en vez de reventar la RAM.
	huge := "<body><p>" + strings.Repeat("palabra ", maxArticleBytes/4) + "</p></body>"
	body := extractText(strings.NewReader(huge))
	if len(body) == 0 {
		t.Fatal("el límite dejó el cuerpo vacío; debería truncar, no descartar")
	}
	if len(body) > maxArticleBytes {
		t.Fatalf("el cuerpo (%d bytes) supera maxArticleBytes: el límite no corta", len(body))
	}
}
