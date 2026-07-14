package zim

// Tests de la Fase B: normalización §21 (tests de mesa del criterio §6) y la
// cascada del índice de títulos §20 (v1 → v0 → legacy → sintético), con búsqueda
// por prefijo determinista e insensible a acentos/mayúsculas.

import (
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeKey(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Árbol", "arbol"},
		{"árbol", "arbol"},
		{"ESPAÑA", "espana"},
		{"señor", "senor"},
		{"Müller", "muller"},
		{"L’Hôpital", "l'hopital"},
		{"agujero negro", "agujero negro"},
		{"Ελλάδα", "ελλαδα"}, // griego: minúsculas + sin acentos
		{"", ""},
		{"123-ABC", "123-abc"},
	}
	for _, c := range cases {
		if got := normalizeKey(c.in); got != c.want {
			t.Errorf("normalizeKey(%q) = %q, esperado %q", c.in, got, c.want)
		}
	}
	// Regla de oro §21: idempotente — normalizar dos veces no cambia nada.
	for _, c := range cases {
		if got := normalizeKey(normalizeKey(c.in)); got != c.want {
			t.Errorf("normalizeKey no es idempotente para %q: %q", c.in, got)
		}
	}
}

// suggestZIM: artículos con acentos + entradas de ruido (M/W/X) que el suggest
// NUNCA debe ofrecer. El listing v1 solo lista los front articles (0..3).
func suggestZIM(t testing.TB, withV1 bool, opts ...builderOpt) []byte {
	t.Helper()
	entries := []tEntry{
		{ns: 'C', path: "Arbol", title: "Árbol", mime: 0, content: []byte("a")},             // 0
		{ns: 'C', path: "Arboleda", title: "Arboleda", mime: 0, content: []byte("b")},       // 1
		{ns: 'C', path: "Armenia", title: "Armenia", mime: 0, content: []byte("c")},         // 2
		{ns: 'C', path: "arbitraje", title: "arbitraje", mime: 0, content: []byte("d")},     // 3
		{ns: 'M', path: "Title", title: "Arboles del mundo", mime: 0, content: []byte("t")}, // 4: ruido
	}
	if withV1 {
		entries = append(entries, tEntry{
			ns: 'X', path: "listing/titleOrdered/v1", mime: 0,
			// front articles por título: arbitraje(3), Arboleda(1), Armenia(2), Árbol(0)
			content: []byte{3, 0, 0, 0, 1, 0, 0, 0, 2, 0, 0, 0, 0, 0, 0, 0},
		})
	}
	return buildZIMC(t, []string{"text/html"}, entries, noMainPage, compNone, false, opts...)
}

func searchTitles(t *testing.T, a Archive, prefix string, limit int) []string {
	t.Helper()
	ti, err := a.TitleIndex()
	if err != nil {
		t.Fatal(err)
	}
	keys, err := ti.Search(prefix, limit)
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, k := range keys {
		e, err := a.EntryAt(k)
		if err != nil {
			t.Fatal(err)
		}
		titles = append(titles, e.Title())
	}
	return titles
}

func TestTitleIndexSearch(t *testing.T) {
	for _, tc := range []struct {
		name   string
		data   []byte
		source string
	}{
		{"desde v1", suggestZIM(t, true), "v1"},
		{"sintético (sin listings)", suggestZIM(t, false), "synthetic"},
		{"legacy titlePtrPos", suggestZIM(t, false, withLegacyTitleList()), "legacy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, err := openZIMBytes(t, tc.data, nil)
			if err != nil {
				t.Fatal(err)
			}

			// "arb" sin acentos encuentra Árbol (con tilde) y ordena por clave
			// normalizada: arbitraje < arbol < arboleda.
			got := searchTitles(t, a, "arb", 10)
			want := []string{"arbitraje", "Árbol", "Arboleda"}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("Search(arb) = %v, esperado %v", got, want)
			}

			// Con acento en la consulta, mismo resultado (§21: misma normalización).
			if got := searchTitles(t, a, "árb", 10); !reflect.DeepEqual(got, want) {
				t.Errorf("Search(árb) = %v, esperado %v", got, want)
			}

			// limit se respeta con orden estable.
			if got := searchTitles(t, a, "arb", 2); !reflect.DeepEqual(got, want[:2]) {
				t.Errorf("Search(arb, 2) = %v", got)
			}

			// El ruido M/* jamás sale, aunque su título case ("Arboles del mundo").
			for _, title := range searchTitles(t, a, "arboles", 10) {
				if title == "Arboles del mundo" {
					t.Error("el suggest ofreció una entrada M/*")
				}
			}

			// Sin resultados ≠ error.
			if got := searchTitles(t, a, "zzz", 10); len(got) != 0 {
				t.Errorf("Search(zzz) = %v", got)
			}

			src := a.(*archive).titleIdx.source
			if src != tc.source {
				t.Errorf("fuente = %q, esperada %q", src, tc.source)
			}
		})
	}
}

func TestTitleIndexInvalidListingFallsBack(t *testing.T) {
	// v1 presente pero corrupto (tamaño no múltiplo de 4 / índice fuera de rango)
	// → la cascada cae al sintético sin error.
	for _, bad := range [][]byte{
		{1, 0, 0},     // 3 bytes
		{99, 0, 0, 0}, // índice 99 con 5 entradas
		{},            // vacío
	} {
		entries := []tEntry{
			{ns: 'C', path: "Arbol", title: "Árbol", mime: 0, content: []byte("a")},
			{ns: 'X', path: "listing/titleOrdered/v1", mime: 0, content: bad},
		}
		data := buildZIM(t, []string{"text/html"}, entries, noMainPage)
		a, err := openZIMBytes(t, data, nil)
		if err != nil {
			t.Fatal(err)
		}
		got := searchTitles(t, a, "arbol", 10)
		if len(got) != 1 || got[0] != "Árbol" {
			t.Errorf("con v1 corrupto %v: Search = %v", bad, got)
		}
		if src := a.(*archive).titleIdx.source; src != "synthetic" {
			t.Errorf("fuente = %q, esperada synthetic", src)
		}
	}
}

func TestTitleIndexDeterministic(t *testing.T) {
	// Mismo ZIM → mismo índice, byte a byte (criterio §6).
	data := suggestZIM(t, true)
	var full0, word0 string
	for i := 0; i < 2; i++ {
		a, err := openZIMBytes(t, data, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.TitleIndex(); err != nil {
			t.Fatal(err)
		}
		ti := a.(*archive).titleIdx
		full := string(ti.fullKeys)
		var wb strings.Builder
		wb.Write(ti.wordKeys)
		for k, x := range ti.wordIdxs {
			wb.WriteByte(ti.wordPos[k])
			wb.WriteByte(byte(x))
		}
		if i == 0 {
			full0, word0 = full, wb.String()
			continue
		}
		if full != full0 || wb.String() != word0 {
			t.Error("dos construcciones del mismo ZIM difieren")
		}
	}
}

func TestTitleIndexWordPrefix(t *testing.T) {
	// Casar el prefijo contra CUALQUIER palabra del título (§6, como kiwix):
	// "einst" debe encontrar "Little Einsteins", no solo títulos que empiezan por
	// "einst". Y el que casa al INICIO va primero (ranking por posición).
	entries := []tEntry{
		{ns: 'C', path: "Albert_Einstein", title: "Albert Einstein", mime: 0, content: []byte("a")},
		{ns: 'C', path: "Einstein", title: "Einstein", mime: 0, content: []byte("b")},
		{ns: 'C', path: "Little_Einsteins", title: "Little Einsteins", mime: 0, content: []byte("c")},
	}
	data := buildZIM(t, []string{"text/html"}, entries, noMainPage)
	a, err := openZIMBytes(t, data, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := searchTitles(t, a, "einst", 10)
	// "Einstein" (pos0) primero; luego los de mitad de título por clave
	// normalizada: "albert einstein" < "little einsteins".
	want := []string{"Einstein", "Albert Einstein", "Little Einsteins"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Search(einst) = %v, esperado %v", got, want)
	}
	// Una entrada no se repite aunque case por varias palabras.
	if got := searchTitles(t, a, "e", 10); len(got) != 3 {
		t.Errorf("Search(e) devolvió %d entradas (esperadas 3 distintas): %v", len(got), got)
	}
}

func TestTitleIndexRedirectsIncluded(t *testing.T) {
	// Un redirect de artículo ("NYC" → "New York City") SÍ se sugiere.
	entries := []tEntry{
		{ns: 'C', path: "NYC", title: "NYC", isRedirect: true, redirect: 1},
		{ns: 'C', path: "New_York_City", title: "New York City", mime: 0, content: []byte("x")},
	}
	data := buildZIM(t, []string{"text/html"}, entries, noMainPage)
	a, err := openZIMBytes(t, data, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := searchTitles(t, a, "nyc", 10)
	if len(got) != 1 || got[0] != "NYC" {
		t.Errorf("Search(nyc) = %v", got)
	}
}
