package zim

// Tests del caché de disco del índice de títulos (titleindex_cache.go): el .tix
// es 100% derivado — CUALQUIER duda (corrupción, truncado, otro ZIM, versión
// vieja) degrada a rebuild, nunca a resultados malos.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// suggestZIMFile escribe el ZIM sintético del suggest a un fichero temporal y lo
// abre por la ruta de producción (Open), que es la que activa el caché.
func suggestZIMFile(t *testing.T, limits Limits) (string, Archive) {
	t.Helper()
	data := suggestZIM(t, true)
	path := filepath.Join(t.TempDir(), "col.zim")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := Open(context.Background(), path, &Options{Limits: &limits})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return path, a
}

func cacheLimits() Limits {
	l := DefaultLimits()
	l.TitleIndexCache = true
	l.SuggestWordIndex = true
	return l
}

// El primer uso construye y persiste; el segundo (archive nuevo) carga del disco
// y busca EXACTAMENTE igual.
func TestTitleCacheRoundTrip(t *testing.T) {
	path, a := suggestZIMFile(t, cacheLimits())

	want := searchTitles(t, a, "arbol", 10)
	if len(want) == 0 {
		t.Fatal("el ZIM sintético debería casar 'arbol'")
	}
	tix := titleCachePath(path)
	if _, err := os.Stat(tix); err != nil {
		t.Fatalf("el primer TitleIndex() debería haber escrito %s: %v", tix, err)
	}

	// Segundo archive sobre el MISMO fichero: debe cargar del caché.
	l := cacheLimits()
	b, err := Open(context.Background(), path, &Options{Limits: &l})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	got := searchTitles(t, b, "arbol", 10)
	if len(got) != len(want) {
		t.Fatalf("resultados distintos con caché: %v vs %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("resultados distintos con caché: %v vs %v", got, want)
		}
	}
	ti, _ := b.TitleIndex()
	if _, _, src := TitleIndexStats(ti); src != "v1+tix" {
		t.Fatalf("la fuente debería marcar el caché (v1+tix), es %q", src)
	}
}

// Un byte corrupto en el cuerpo → CRC no casa → rebuild transparente.
func TestTitleCacheCorruptionFallsBack(t *testing.T) {
	path, a := suggestZIMFile(t, cacheLimits())
	if _, err := a.TitleIndex(); err != nil {
		t.Fatal(err)
	}

	tix := titleCachePath(path)
	raw, err := os.ReadFile(tix)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)/2] ^= 0xFF
	if err := os.WriteFile(tix, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	l := cacheLimits()
	b, err := Open(context.Background(), path, &Options{Limits: &l})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if got := searchTitles(t, b, "arbol", 10); len(got) == 0 {
		t.Fatal("con caché corrupto debería reconstruir y seguir buscando")
	}
	ti, _ := b.TitleIndex()
	if _, _, src := TitleIndexStats(ti); src != "v1" {
		t.Fatalf("con caché corrupto la fuente debería ser rebuild (v1), es %q", src)
	}
}

// Truncado (corte a mitad de escritura sin rename... o disco roto) → rebuild.
func TestTitleCacheTruncatedFallsBack(t *testing.T) {
	path, a := suggestZIMFile(t, cacheLimits())
	if _, err := a.TitleIndex(); err != nil {
		t.Fatal(err)
	}
	tix := titleCachePath(path)
	raw, _ := os.ReadFile(tix)
	if err := os.WriteFile(tix, raw[:len(raw)/3], 0o644); err != nil {
		t.Fatal(err)
	}

	l := cacheLimits()
	b, err := Open(context.Background(), path, &Options{Limits: &l})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if got := searchTitles(t, b, "arbol", 10); len(got) == 0 {
		t.Fatal("con caché truncado debería reconstruir y seguir buscando")
	}
}

// El caché de OTRO ZIM (uuid distinto) jamás se acepta: resultados fantasma no.
func TestTitleCacheRejectsForeignZim(t *testing.T) {
	// readTix directamente: uuid esperado ≠ uuid grabado.
	dir := t.TempDir()
	ti := &titleIndex{
		source:   "v1",
		fullKeys: []byte("arbol"),
		fullOffs: []uint32{0, 5},
		fullIdxs: []uint32{0},
	}
	var uuidA, uuidB [16]byte
	uuidA[0], uuidB[0] = 0xAA, 0xBB
	p := filepath.Join(dir, "x.tix")
	if err := writeTix(p, uuidA, 5, ti); err != nil {
		t.Fatal(err)
	}
	if _, err := readTix(p, uuidB, 5, false); err == nil {
		t.Fatal("un .tix de otro ZIM debería rechazarse")
	}
	// Y el mismo uuid con entryCount distinto, también.
	if _, err := readTix(p, uuidA, 6, false); err == nil {
		t.Fatal("entryCount distinto debería rechazarse")
	}
	// El bueno, por sanidad, carga.
	if _, err := readTix(p, uuidA, 5, false); err != nil {
		t.Fatalf("el .tix correcto debería cargar: %v", err)
	}
}

// Caché escrito sin índice de palabras + ejecución que SÍ lo quiere → rebuild.
// Al revés (caché con palabras, ejecución sin) → carga y las ignora.
func TestTitleCacheWordFlagMismatch(t *testing.T) {
	noWords := cacheLimits()
	noWords.SuggestWordIndex = false
	path, a := suggestZIMFile(t, noWords)
	if _, err := a.TitleIndex(); err != nil {
		t.Fatal(err)
	}

	// La misma ruta con palabras pedidas: el caché sin palabras no vale.
	withWords := cacheLimits()
	b, err := Open(context.Background(), path, &Options{Limits: &withWords})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	ti, err := b.TitleIndex()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, src := TitleIndexStats(ti); src != "v1" {
		t.Fatalf("caché sin palabras + config con palabras debería reconstruir, fuente %q", src)
	}

	// Ahora el caché quedó reescrito CON palabras; una ejecución sin palabras lo
	// carga igual (ignorándolas).
	c, err := Open(context.Background(), path, &Options{Limits: &noWords})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	tic, err := c.TitleIndex()
	if err != nil {
		t.Fatal(err)
	}
	full, word, src := TitleIndexStats(tic)
	if src != "v1+tix" {
		t.Fatalf("caché con palabras + config sin debería cargar del disco, fuente %q", src)
	}
	if full == 0 || word != 0 {
		t.Fatalf("debería cargar solo el índice completo (full=%d word=%d)", full, word)
	}
}

// ZIM_TITLE_CACHE=0 (TitleIndexCache=false): ni lee ni escribe .tix.
func TestTitleCacheDisabled(t *testing.T) {
	off := cacheLimits()
	off.TitleIndexCache = false
	path, a := suggestZIMFile(t, off)
	if _, err := a.TitleIndex(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(titleCachePath(path)); !os.IsNotExist(err) {
		t.Fatal("con el caché desactivado no debería escribirse .tix")
	}
}
