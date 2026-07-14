package zim

// Persistencia del índice de títulos (§20, enmienda 2026-07-14): el índice de la
// Wikipedia ES (4.3M artículos) tarda ~20 s en construirse — pagarlo en CADA
// arranque hacía la primera búsqueda insufrible. Se vuelca a disco UNA vez
// (`<zim>.tix`, junto al fichero, misma convención que el `.bleve` del FTS) y los
// arranques siguientes lo cargan en una fracción del tiempo.
//
// El caché es 100% derivado y prescindible: si falta, no valida o está corrupto,
// se reconstruye desde el ZIM y se reescribe. Nunca es fuente de verdad.
//
// Validación al cargar (el fichero puede estar truncado, corrupto o ser de otro
// ZIM): magic + versión de formato, UUID + entryCount del ZIM, CRC32 del
// contenido completo, y estructura (offsets monotónicos, índices en rango). Ante
// CUALQUIER duda → error → rebuild. Mismo espíritu que el manifiesto del FTS.

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// tixMagic identifica el formato; tixVersion se INCREMENTA si cambia el layout
// del fichero O cualquier pieza que determine el contenido del índice: la
// normalización §21 (normalizeKey), el tokenizador (titleWords), maxTitleWords o
// isArticleForSuggest. Un caché de versión distinta se descarta y reconstruye.
const (
	tixMagic   = "ZTIX"
	tixVersion = 1
)

// tixLimits: topes de cordura al cargar (§16 aplica también a nuestros propios
// ficheros: un .tix corrupto no puede pedir asignaciones absurdas).
const (
	tixMaxKeysBytes = 2 << 30 // 2 GiB de claves por índice
	tixMaxRows      = 1 << 28 // 268M filas
	tixMaxSourceLen = 32
)

// titleCachePath: `<zim sin extensión>.tix`, al lado del fichero (como `.bleve`).
func titleCachePath(zimPath string) string {
	return strings.TrimSuffix(zimPath, filepath.Ext(zimPath)) + ".tix"
}

// loadTitleIndexCache intenta cargar el índice desde disco. (nil, nil) significa
// "no hay caché utilizable" SIN error grave: caché desactivado, archive
// in-memory, fichero ausente… El caller decide reconstruir. Un error de
// validación también degrada a (nil, nil): el rebuild es siempre la red.
func (a *archive) loadTitleIndexCache() *titleIndex {
	if !a.limits.TitleIndexCache || a.path == "" {
		return nil
	}
	ti, err := readTix(titleCachePath(a.path), a.hdr.uuid, a.hdr.entryCount, a.limits.SuggestWordIndex)
	if err != nil {
		return nil // ausente/corrupto/de otro ZIM: reconstruir (y re-guardar)
	}
	ti.a = a
	return ti
}

// saveTitleIndexCache persiste el índice recién construido. Best-effort: un pool
// de solo lectura o sin espacio solo significa que no habrá caché — el índice en
// RAM funciona igual. Escritura atómica: tmp + rename, así un corte a mitad deja
// un tmp huérfano reconocible, nunca un .tix a medias con nombre bueno.
func (a *archive) saveTitleIndexCache(ti *titleIndex) {
	if !a.limits.TitleIndexCache || a.path == "" || ti == nil {
		return
	}
	final := titleCachePath(a.path)
	tmp := fmt.Sprintf("%s.tmp-%d", final, os.Getpid())
	if err := writeTix(tmp, a.hdr.uuid, a.hdr.entryCount, ti); err != nil {
		os.Remove(tmp)
		return
	}
	os.Remove(final) // Windows: rename no pisa; la ventana la cubre el rebuild
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
	}
}

// ── Formato en disco ─────────────────────────────────────────────────────────
//
//	magic "ZTIX" · u32 version · uuid[16] · u32 entryCount · u8 flags(bit0=word)
//	u8 len(source) · source
//	u64 len(fullKeys) · u32 nFull · fullKeys · (nFull+1)×u32 offs · nFull×u32 idxs
//	[si flags&1] u64 len(wordKeys) · u32 nWord · wordKeys · (nWord+1)×u32 offs ·
//	             nWord×u32 idxs · nWord×u8 pos
//	u32 CRC32(IEEE) de TODO lo anterior
//
// Todo little-endian. El CRC cierra el fichero: truncado o bit podrido → no casa.

func writeTix(path string, uuid [16]byte, entryCount uint32, ti *titleIndex) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	crc := crc32.NewIEEE()
	bw := bufio.NewWriterSize(io.MultiWriter(f, crc), 1<<20)

	w := &tixWriter{w: bw}
	w.bytes([]byte(tixMagic))
	w.u32(tixVersion)
	w.bytes(uuid[:])
	w.u32(entryCount)
	flags := byte(0)
	if len(ti.wordOffs) > 0 {
		flags |= 1
	}
	w.u8(flags)
	w.u8(uint8(len(ti.source)))
	w.bytes([]byte(ti.source))

	w.u64(uint64(len(ti.fullKeys)))
	w.u32(uint32(len(ti.fullIdxs)))
	w.bytes(ti.fullKeys)
	w.u32s(ti.fullOffs)
	w.u32s(ti.fullIdxs)

	if flags&1 != 0 {
		w.u64(uint64(len(ti.wordKeys)))
		w.u32(uint32(len(ti.wordIdxs)))
		w.bytes(ti.wordKeys)
		w.u32s(ti.wordOffs)
		w.u32s(ti.wordIdxs)
		w.bytes(ti.wordPos)
	}
	if w.err != nil {
		return w.err
	}
	if err := bw.Flush(); err != nil {
		return err
	}
	// El CRC va FUERA del propio hash, al final del fichero.
	var tail [4]byte
	binary.LittleEndian.PutUint32(tail[:], crc.Sum32())
	if _, err := f.Write(tail[:]); err != nil {
		return err
	}
	return f.Sync() // el rename de arriba solo publica un fichero ya en disco
}

func readTix(path string, uuid [16]byte, entryCount uint32, wantWord bool) (*titleIndex, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.Size() < 4 {
		return nil, fmt.Errorf("tix: demasiado corto")
	}

	crc := crc32.NewIEEE()
	r := &tixReader{r: bufio.NewReaderSize(io.TeeReader(io.LimitReader(f, st.Size()-4), crc), 1<<20)}

	if string(r.bytes(4)) != tixMagic {
		return nil, fmt.Errorf("tix: magic inválido")
	}
	if v := r.u32(); v != tixVersion {
		return nil, fmt.Errorf("tix: versión %d ≠ %d", v, tixVersion)
	}
	var u [16]byte
	copy(u[:], r.bytes(16))
	if u != uuid {
		return nil, fmt.Errorf("tix: es de otro ZIM")
	}
	if ec := r.u32(); ec != entryCount {
		return nil, fmt.Errorf("tix: entryCount %d ≠ %d", ec, entryCount)
	}
	flags := r.u8()
	srcLen := int(r.u8())
	if srcLen > tixMaxSourceLen {
		return nil, fmt.Errorf("tix: source de %d bytes", srcLen)
	}
	source := string(r.bytes(srcLen))
	hasWord := flags&1 != 0
	if wantWord && !hasWord {
		// El caché se escribió sin índice de palabras (ZIM_SUGGEST_WORDS=0) y esta
		// ejecución lo quiere: reconstruir. Al revés sí vale: se carga y se ignora.
		return nil, fmt.Errorf("tix: sin índice de palabras y la config lo pide")
	}

	ti := &titleIndex{source: source + "+tix"}

	loadPart := func(withPos bool) ([]byte, []uint32, []uint32, []byte, error) {
		keysLen := r.u64()
		n := int(r.u32())
		if keysLen > tixMaxKeysBytes || n > tixMaxRows || r.err != nil {
			return nil, nil, nil, nil, fmt.Errorf("tix: tamaños fuera de rango")
		}
		keys := r.bytes(int(keysLen))
		offs := r.u32s(n + 1)
		idxs := r.u32s(n)
		var pos []byte
		if withPos {
			pos = r.bytes(n)
		}
		if r.err != nil {
			return nil, nil, nil, nil, r.err
		}
		// Estructura: offsets monotónicos que cierran exactamente sobre las claves,
		// e índices dentro de la path pointer list de ESTE ZIM.
		if len(offs) == 0 || offs[0] != 0 || uint64(offs[n]) != keysLen {
			return nil, nil, nil, nil, fmt.Errorf("tix: offsets no cierran")
		}
		for i := 1; i <= n; i++ {
			if offs[i] < offs[i-1] {
				return nil, nil, nil, nil, fmt.Errorf("tix: offsets no monotónicos")
			}
		}
		for _, ix := range idxs {
			if ix >= entryCount {
				return nil, nil, nil, nil, fmt.Errorf("tix: idx %d fuera de rango", ix)
			}
		}
		return keys, offs, idxs, pos, nil
	}

	var errPart error
	if ti.fullKeys, ti.fullOffs, ti.fullIdxs, _, errPart = loadPart(false); errPart != nil {
		return nil, errPart
	}
	if hasWord {
		wk, wo, wi, wp, err := loadPart(true)
		if err != nil {
			return nil, err
		}
		if wantWord {
			ti.wordKeys, ti.wordOffs, ti.wordIdxs, ti.wordPos = wk, wo, wi, wp
		} // si no se quiere, se ha leído (por el CRC) pero no se retiene
	}

	// Nada debe sobrar antes del CRC…
	if _, err := r.r.ReadByte(); err != io.EOF {
		return nil, fmt.Errorf("tix: datos de más")
	}
	// …y el CRC del contenido debe casar con el del final del fichero.
	var tail [4]byte
	if _, err := f.ReadAt(tail[:], st.Size()-4); err != nil {
		return nil, err
	}
	if binary.LittleEndian.Uint32(tail[:]) != crc.Sum32() {
		return nil, fmt.Errorf("tix: CRC no casa (fichero corrupto)")
	}
	return ti, nil
}

// ── Lectura/escritura little-endian con error pegajoso ───────────────────────

type tixWriter struct {
	w   *bufio.Writer
	err error
	buf [8]byte
}

func (w *tixWriter) bytes(b []byte) {
	if w.err == nil {
		_, w.err = w.w.Write(b)
	}
}
func (w *tixWriter) u8(v uint8) {
	if w.err == nil {
		w.err = w.w.WriteByte(v)
	}
}
func (w *tixWriter) u32(v uint32) {
	binary.LittleEndian.PutUint32(w.buf[:4], v)
	w.bytes(w.buf[:4])
}
func (w *tixWriter) u64(v uint64) {
	binary.LittleEndian.PutUint64(w.buf[:8], v)
	w.bytes(w.buf[:8])
}
func (w *tixWriter) u32s(s []uint32) {
	if w.err != nil {
		return
	}
	// Codificación por bloques: millones de filas sin un Write por elemento.
	var chunk [4096]byte
	for len(s) > 0 {
		n := len(s)
		if n > len(chunk)/4 {
			n = len(chunk) / 4
		}
		for i := 0; i < n; i++ {
			binary.LittleEndian.PutUint32(chunk[i*4:], s[i])
		}
		w.bytes(chunk[:n*4])
		s = s[n:]
	}
}

type tixReader struct {
	r   *bufio.Reader
	err error
}

func (r *tixReader) bytes(n int) []byte {
	if r.err != nil || n < 0 {
		return nil
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r.r, b); err != nil {
		r.err = err
		return nil
	}
	return b
}
func (r *tixReader) u8() uint8 {
	b := r.bytes(1)
	if b == nil {
		return 0
	}
	return b[0]
}
func (r *tixReader) u32() uint32 {
	b := r.bytes(4)
	if b == nil {
		return 0
	}
	return binary.LittleEndian.Uint32(b)
}
func (r *tixReader) u64() uint64 {
	b := r.bytes(8)
	if b == nil {
		return 0
	}
	return binary.LittleEndian.Uint64(b)
}
func (r *tixReader) u32s(n int) []uint32 {
	b := r.bytes(n * 4)
	if b == nil {
		return nil
	}
	s := make([]uint32, n)
	for i := range s {
		s[i] = binary.LittleEndian.Uint32(b[i*4:])
	}
	return s
}
