package zim

// Índice de títulos (Fase B, §3.6 + §20): suggest por prefijo, determinista y
// tolerante a acentos/mayúsculas.
//
// Cascada de FUENTES de candidatos, con validación estructural en cada nivel:
//
//	1. X/listing/titleOrdered/v1  (solo "front articles" — lo que suggest quiere)
//	2. X/listing/titleOrdered/v0  (todas las entradas; se filtra a artículos)
//	3. titlePtrPos del header     (legacy; libzim 9.3 ya no la escribe)
//	4. índice sintético           (recorrer todos los dirents; el caso raro)
//
// DESVIACIÓN deliberada de §20: el orden EN DISCO de estas listas no se valida ni
// se usa — solo aportan el CONJUNTO de candidatos. El índice se reordena SIEMPRE en
// memoria con la normalización §21, porque una búsqueda por prefijo insensible a
// acentos exige ese orden, no el collation de libzim. Consecuencia gratis: mismo
// ZIM → mismo índice, byte a byte (criterio §6), venga del nivel que venga.
//
// Construcción perezosa (primer TitleIndex()) y cacheada: leer y normalizar los
// títulos de un ZIM grande cuesta segundos — solo se paga si el suggest se usa, y
// una vez. La persistencia fuera del ZIM queda DIFERIDA (§20, enmienda).

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode"
)

// titleIndex: DOS índices compactos, para casar el prefijo contra cualquier
// palabra del título (como el índice tokenizado de kiwix, §6) sin el coste de RAM
// de indexar sufijos completos (medido: 1.5 GB en la Wikipedia ES — inviable en la
// Pi; los sufijos duplican el texto en O(palabras²)).
//
//   - full:  títulos completos normalizados, ordenados. Cubre el prefijo
//     multi-palabra ("agujero ne") y la coincidencia al INICIO ("einst"→"Einstein").
//   - word:  palabras NO iniciales (pos>0) de cada título, ordenadas. Cubre la
//     coincidencia a mitad de título ("einst"→"Little Einsteins"). La palabra
//     inicial no se guarda: ya la cubre `full`. Cada título aporta su texto ~una
//     vez, no al cuadrado.
//
// La unión de ambos, rankeada por posición de palabra (0 = inicio), reproduce el
// suggest de kiwix: coincidencias al inicio primero, luego las de mitad de título.
type titleIndex struct {
	a *archive

	fullKeys []byte
	fullOffs []uint32
	fullIdxs []uint32

	wordKeys []byte
	wordOffs []uint32
	wordIdxs []uint32
	wordPos  []uint8

	source string // "v1", "v0", "legacy", "synthetic" — métricas/depuración
}

// maxTitleWords: tope de palabras indexadas por título (§16, contra títulos
// absurdos). Los reales tienen pocas; 16 cubre de sobra.
const maxTitleWords = 16

// titleWords parte un título normalizado en palabras rompiendo en CUALQUIER
// carácter no alfanumérico (no solo espacios), igual que el tokenizador de kiwix:
// "portal:dinosaurs/dinosaurs" → [portal, dinosaurs, dinosaurs], "atlanta, georgia"
// → [atlanta, georgia]. Así el prefijo casa contra palabras separadas por ":", "/",
// ",", "(", etc.
func titleWords(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// TitleIndexStats expone el tamaño del índice para medición/telemetría (§24).
// Devuelve (filas del índice completo, filas del índice de palabras, fuente).
func TitleIndexStats(ti TitleIndex) (full, word int, source string) {
	t, ok := ti.(*titleIndex)
	if !ok {
		return 0, 0, ""
	}
	return len(t.fullIdxs), len(t.wordIdxs), t.source
}

func keyAt(buf []byte, offs []uint32, i int) string {
	return string(buf[offs[i]:offs[i+1]])
}

// Search (§6): prefijo normalizado → hasta limit entradas distintas. Une las
// coincidencias de `full` (posición 0) y `word` (posición >0), dedup por entrada
// quedándose con la mejor posición, y ordena por (posición, clave, idx) — las
// coincidencias al inicio del título primero. Determinista.
func (t *titleIndex) Search(prefix string, limit int) ([]EntryKey, error) {
	if limit <= 0 {
		return nil, nil
	}
	p := normalizeKey(prefix)
	if p == "" {
		return nil, nil
	}

	type cand struct {
		idx uint32
		pos uint8
		key string
	}
	rawCap := limit * 40     // tope de barrido por índice (prefijos cortos casan mucho)
	distinctCap := limit * 4 // candidatos distintos antes de rankear

	var out []EntryKey
	err := t.a.withReader(func() error {
		best := make(map[uint32]int)
		cands := make([]cand, 0, distinctCap)
		add := func(idx uint32, pos uint8, key string) {
			if j, ok := best[idx]; ok {
				if pos < cands[j].pos {
					cands[j].pos, cands[j].key = pos, key
				}
				return
			}
			best[idx] = len(cands)
			cands = append(cands, cand{idx: idx, pos: pos, key: key})
		}

		// full → posición 0.
		nf := len(t.fullIdxs)
		lo := sort.Search(nf, func(i int) bool { return keyAt(t.fullKeys, t.fullOffs, i) >= p })
		for i := lo; i < nf && i < lo+rawCap && len(best) < distinctCap; i++ {
			k := keyAt(t.fullKeys, t.fullOffs, i)
			if !strings.HasPrefix(k, p) {
				break
			}
			add(t.fullIdxs[i], 0, k)
		}
		// word → posición >0.
		nw := len(t.wordIdxs)
		lw := sort.Search(nw, func(i int) bool { return keyAt(t.wordKeys, t.wordOffs, i) >= p })
		for i := lw; i < nw && i < lw+rawCap && len(best) < distinctCap*2; i++ {
			k := keyAt(t.wordKeys, t.wordOffs, i)
			if !strings.HasPrefix(k, p) {
				break
			}
			add(t.wordIdxs[i], t.wordPos[i], k)
		}

		sort.Slice(cands, func(a, b int) bool {
			if cands[a].pos != cands[b].pos {
				return cands[a].pos < cands[b].pos
			}
			if cands[a].key != cands[b].key {
				return cands[a].key < cands[b].key
			}
			return cands[a].idx < cands[b].idx
		})
		if len(cands) > limit {
			cands = cands[:limit]
		}
		for _, c := range cands {
			d, err := t.a.direntAtIndex(c.idx)
			if err != nil {
				return err
			}
			out = append(out, d.key())
		}
		return nil
	})
	return out, err
}

// buildTitleIndex: cascada §20. Cada nivel que no valide se ANOTA y se cae al
// siguiente; el sintético no puede fallar (como mucho queda vacío).
func (a *archive) buildTitleIndex() (*titleIndex, error) {
	var candidates []uint32
	source := ""
	for _, lv := range []struct {
		name string
		get  func() ([]uint32, error)
	}{
		{"v1", func() ([]uint32, error) {
			return a.listingIndices(EntryKey{'X', "listing/titleOrdered/v1"})
		}},
		{"v0", func() ([]uint32, error) {
			return a.listingIndices(EntryKey{'X', "listing/titleOrdered/v0"})
		}},
		{"legacy", a.legacyTitleIndices},
	} {
		if idxs, err := lv.get(); err == nil {
			candidates, source = idxs, lv.name
			break
		}
	}
	if source == "" {
		// Sintético (§20): todos los dirents, de uno en uno. El caso raro.
		candidates = make([]uint32, a.hdr.entryCount)
		for i := range candidates {
			candidates[i] = uint32(i)
		}
		source = "synthetic"
	}

	recs, err := a.collectTitleRecs(candidates)
	if err != nil {
		return nil, err
	}

	ti := &titleIndex{a: a, source: source}

	// full: un título completo por entrada. Orden §21, desempate por idx.
	sort.Slice(recs, func(i, j int) bool {
		if recs[i].key != recs[j].key {
			return recs[i].key < recs[j].key
		}
		return recs[i].idx < recs[j].idx
	})
	ti.fullOffs = make([]uint32, 1, len(recs)+1)
	ti.fullIdxs = make([]uint32, 0, len(recs))
	for _, r := range recs {
		ti.fullKeys = append(ti.fullKeys, r.key...)
		ti.fullOffs = append(ti.fullOffs, uint32(len(ti.fullKeys)))
		ti.fullIdxs = append(ti.fullIdxs, r.idx)
	}

	// word: palabras NO iniciales de cada título, dedup por (palabra) dentro del
	// título quedándose con la primera aparición (menor posición > 0). Opt-out por
	// RAM en la Pi (ZIM_SUGGEST_WORDS=0): sin él, el suggest es prefijo del título.
	if !a.limits.SuggestWordIndex {
		return ti, nil
	}
	type wrow struct {
		key string
		idx uint32
		pos uint8
	}
	words := make([]wrow, 0, len(recs)*2)
	for _, r := range recs {
		ws := titleWords(r.key)
		if len(ws) > maxTitleWords {
			ws = ws[:maxTitleWords]
		}
		seen := make(map[string]bool, len(ws))
		for wi := 1; wi < len(ws); wi++ { // wi>=1: la palabra inicial la cubre `full`
			w := ws[wi]
			if seen[w] {
				continue
			}
			seen[w] = true
			words = append(words, wrow{key: w, idx: r.idx, pos: uint8(wi)})
		}
	}
	sort.Slice(words, func(i, j int) bool {
		if words[i].key != words[j].key {
			return words[i].key < words[j].key
		}
		if words[i].pos != words[j].pos {
			return words[i].pos < words[j].pos
		}
		return words[i].idx < words[j].idx
	})
	ti.wordOffs = make([]uint32, 1, len(words)+1)
	ti.wordIdxs = make([]uint32, 0, len(words))
	ti.wordPos = make([]uint8, 0, len(words))
	for _, r := range words {
		ti.wordKeys = append(ti.wordKeys, r.key...)
		ti.wordOffs = append(ti.wordOffs, uint32(len(ti.wordKeys)))
		ti.wordIdxs = append(ti.wordIdxs, r.idx)
		ti.wordPos = append(ti.wordPos, r.pos)
	}
	return ti, nil
}

type titleRec struct {
	key string
	idx uint32
}

// collectTitleRecs lee el título de cada candidato con I/O SECUENCIAL, no con un
// ReadAt por dirent: para la Wikipedia ES son millones de candidatos, y el patrón
// aleatorio (puntero + dirent + strings por candidato) es una tormenta de syscalls
// en x86 y la muerte en el almacenamiento de la Pi. En su lugar:
//
//  1. la path pointer list se recorre en chunks de 1 MiB (una pasada),
//  2. los (offset, idx) se ordenan por offset de dirent,
//  3. los dirents se parsean desde ventanas de 8 MiB leídas en orden.
func (a *archive) collectTitleRecs(candidates []uint32) ([]titleRec, error) {
	dataEnd := int64(a.hdr.checksumPos)

	type cand struct {
		ptr int64
		idx uint32
	}
	cands := make([]cand, 0, len(candidates))

	err := a.withReader(func() error {
		// 1. Punteros de los candidatos, barriendo la pointer list por chunks.
		sorted := append([]uint32(nil), candidates...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

		const chunkEntries = 128 << 10 // 1 MiB de punteros por lectura
		buf := make([]byte, chunkEntries*8)
		for k := 0; k < len(sorted); {
			base := sorted[k] // primer candidato pendiente
			n := int64(chunkEntries)
			if rem := int64(a.hdr.entryCount) - int64(base); rem < n {
				n = rem
			}
			chunk := buf[:n*8]
			if _, err := a.f.ReadAt(chunk, int64(a.hdr.pathPtrPos)+8*int64(base)); err != nil {
				return fmt.Errorf("%w: leyendo path pointers: %v", ErrCorrupt, err)
			}
			for k < len(sorted) && int64(sorted[k]) < int64(base)+n {
				ptr := int64(binary.LittleEndian.Uint64(chunk[(sorted[k]-base)*8:]))
				if ptr < headerSize || ptr >= dataEnd {
					return fmt.Errorf("%w: path pointer %d → %d fuera de rango", ErrCorrupt, sorted[k], ptr)
				}
				// Duplicados en un listing hostil: el mismo idx dos veces solo
				// duplicaría el resultado; se colapsan aquí.
				if len(cands) == 0 || cands[len(cands)-1].idx != sorted[k] {
					cands = append(cands, cand{ptr: ptr, idx: sorted[k]})
				}
				k++
			}
		}

		// 2. Orden por offset de dirent → la pasada 3 es I/O hacia delante.
		sort.Slice(cands, func(i, j int) bool { return cands[i].ptr < cands[j].ptr })
		return nil
	})
	if err != nil {
		return nil, err
	}

	// 3. Ventana deslizante sobre la zona de dirents.
	recs := make([]titleRec, 0, len(cands))
	err = a.withReader(func() error {
		const winSize = 8 << 20
		win := make([]byte, winSize)
		var base, winLen int64 = -1, 0
		for _, c := range cands {
			for {
				if base >= 0 && c.ptr >= base && c.ptr < base+winLen {
					d, err := a.parseDirent(win[c.ptr-base:winLen], base+winLen < dataEnd)
					if err == nil {
						if isArticleForSuggest(d) {
							recs = append(recs, titleRec{key: normalizeKey(d.title), idx: c.idx})
						}
						break
					}
					if !errors.Is(err, errShortDirent) {
						// Un candidato roto no tumba el índice entero: se salta.
						// La matriz §25 garantiza el error tipado en el lookup.
						break
					}
					if c.ptr == base {
						// La ventana ya empieza en el dirent y sigue corto: los
						// límites §16 lo hacen imposible salvo corrupción.
						break
					}
				}
				// (Re)llenar la ventana desde el dirent pendiente.
				n := int64(winSize)
				if rem := dataEnd - c.ptr; rem < n {
					n = rem
				}
				if n <= 0 {
					return fmt.Errorf("%w: dirent en %d fuera de los datos", ErrCorrupt, c.ptr)
				}
				if _, err := a.f.ReadAt(win[:n], c.ptr); err != nil {
					return fmt.Errorf("%w: leyendo zona de dirents: %v", ErrCorrupt, err)
				}
				base, winLen = c.ptr, n
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return recs, nil
}

// isArticleForSuggest: lo que el suggest debe ofrecer — contenido y redirects de
// los namespaces de artículo ('C' moderno, 'A' legacy) con título no vacío. Fuera
// M/W/X, imágenes legacy ('I') y linktarget/deleted.
func isArticleForSuggest(d dirent) bool {
	if d.kind != direntContent && d.kind != direntRedirect {
		return false
	}
	if d.namespace != 'C' && d.namespace != 'A' {
		return false
	}
	return d.title != ""
}

// listingIndices lee y valida un listing X/listing/titleOrdered/* (§20): blob de
// u32 (índices en la path pointer list), tamaño múltiplo de 4, índices en rango,
// al menos una entrada cuando el archivo tiene entradas.
func (a *archive) listingIndices(key EntryKey) ([]uint32, error) {
	e, err := a.EntryAt(key)
	if err != nil {
		return nil, err
	}
	rc, info, err := e.Open(context.Background())
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, info.Size))
	if err != nil {
		return nil, err
	}
	return a.validateTitleList(data)
}

// legacyTitleIndices: la title pointer list del header (v0 histórica). libzim 9.3
// puede dejarla a 0 o inválida — de ahí la validación antes de fiarse (§20).
func (a *archive) legacyTitleIndices() ([]uint32, error) {
	pos, n := a.hdr.titlePtrPos, uint64(a.hdr.entryCount)
	if pos < headerSize || n > (a.hdr.checksumPos-pos)/4 {
		return nil, fmt.Errorf("%w: titlePtrPos legacy inválida", ErrCorrupt)
	}
	data := make([]byte, n*4)
	if _, err := a.f.ReadAt(data, int64(pos)); err != nil {
		return nil, fmt.Errorf("%w: leyendo title list legacy: %v", ErrCorrupt, err)
	}
	return a.validateTitleList(data)
}

func (a *archive) validateTitleList(data []byte) ([]uint32, error) {
	if len(data)%4 != 0 || len(data) == 0 {
		return nil, fmt.Errorf("%w: title list de %d bytes (no es lista de u32)", ErrCorrupt, len(data))
	}
	idxs := make([]uint32, len(data)/4)
	for i := range idxs {
		v := binary.LittleEndian.Uint32(data[i*4:])
		if v >= a.hdr.entryCount {
			return nil, fmt.Errorf("%w: title list apunta al dirent %d (entryCount=%d)",
				ErrCorrupt, v, a.hdr.entryCount)
		}
		idxs[i] = v
	}
	return idxs, nil
}
