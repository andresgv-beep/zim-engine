package zim

// Constructor de ZIMs sintéticos para tests: escribe un .zim byte a byte siguiendo
// §3 de ZIM-ENGINE.md (spec contrastada con el ZIM File Example oficial de openZIM).
// Es la "spec hecha código" del arnés §9: si el builder y el parser discrepan, uno
// de los dos está leyendo mal la spec. Los tests de corrupción parten de estos
// bytes válidos y los mutan.
//
// Layout que emite (todas las posiciones van declaradas en el header, el orden es
// el canónico): header · MIME list · dirents · path ptr list · cluster ptr list ·
// 1 cluster · MD5.

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/binary"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

type tEntry struct {
	ns         byte
	path       string
	title      string // "" ⇒ se escribe vacío en disco (el parser debe caer a path)
	mime       uint16 // índice en la MIME list (solo content)
	content    []byte // blob (solo content); va al cluster 0 en orden de aparición
	isRedirect bool
	redirect   uint32 // índice de dirent destino (solo redirect)
	blobAdd    uint32 // se suma al blob asignado — para fabricar blobs fuera de rango
}

// Opciones extra del builder (variádicas para no tocar a los llamantes).
type builderCfg struct {
	legacyTitleList bool // emitir la title pointer list del header (v0 histórica)
}

type builderOpt func(*builderCfg)

func withLegacyTitleList() builderOpt { return func(c *builderCfg) { c.legacyTitleList = true } }

// buildZIM: ZIM con el cluster sin compresión (el caso base de los tests).
func buildZIM(t testing.TB, mimes []string, entries []tEntry, mainPage uint32) []byte {
	t.Helper()
	return buildZIMC(t, mimes, entries, mainPage, compNone, false)
}

// buildZIMC construye un ZIM major 6 completo y coherente, con la compresión y el
// flag extended (offsets u64) que se pidan. entries DEBE venir ordenado por
// (ns, path) — el builder lo verifica en vez de reordenar, para que los índices de
// redirect que escribe el test sean los finales.
func buildZIMC(t testing.TB, mimes []string, entries []tEntry, mainPage uint32, compression byte, extended bool, opts ...builderOpt) []byte {
	t.Helper()
	var cfg builderCfg
	for _, o := range opts {
		o(&cfg)
	}
	for i := 1; i < len(entries); i++ {
		p, q := entries[i-1], entries[i]
		if p.ns > q.ns || (p.ns == q.ns && p.path >= q.path) {
			t.Fatalf("tEntry sin ordenar: %c/%s >= %c/%s", p.ns, p.path, q.ns, q.path)
		}
	}

	le := binary.LittleEndian
	u16 := func(b []byte, v uint16) []byte { var x [2]byte; le.PutUint16(x[:], v); return append(b, x[:]...) }
	u32 := func(b []byte, v uint32) []byte { var x [4]byte; le.PutUint32(x[:], v); return append(b, x[:]...) }
	u64 := func(b []byte, v uint64) []byte { var x [8]byte; le.PutUint64(x[:], v); return append(b, x[:]...) }

	// MIME list (§3.2): strings \0-terminados + string vacío final.
	var mimeList []byte
	for _, m := range mimes {
		mimeList = append(mimeList, m...)
		mimeList = append(mimeList, 0)
	}
	mimeList = append(mimeList, 0)

	// Blobs: los content entries reciben blob 0..n−1 del cluster 0 en orden.
	var blobs [][]byte
	blobOf := make(map[int]uint32)
	for i, e := range entries {
		if !e.isRedirect {
			blobOf[i] = uint32(len(blobs))
			blobs = append(blobs, e.content)
		}
	}

	// Dirents (§3.3), anotando el offset de cada uno.
	direntsStart := uint64(headerSize + len(mimeList))
	var dirents []byte
	direntOff := make([]uint64, len(entries))
	for i, e := range entries {
		direntOff[i] = direntsStart + uint64(len(dirents))
		var d []byte
		if e.isRedirect {
			d = u16(d, mimeRedirect)
			d = append(d, 0, e.ns) // parameter len, namespace
			d = u32(d, 0)          // revision
			d = u32(d, e.redirect)
		} else {
			d = u16(d, e.mime)
			d = append(d, 0, e.ns)
			d = u32(d, 0) // revision
			d = u32(d, 0) // cluster 0 (el único)
			d = u32(d, blobOf[i]+e.blobAdd)
		}
		d = append(d, e.path...)
		d = append(d, 0)
		d = append(d, e.title...)
		d = append(d, 0)
		dirents = append(dirents, d...)
	}

	// Listas de punteros.
	pathPtrPos := direntsStart + uint64(len(dirents))
	var pathPtrs []byte
	for i := range entries {
		pathPtrs = u64(pathPtrs, direntOff[i])
	}

	// Title pointer list legacy (opcional): u32 por entrada, orden por título.
	titlePtrPos := uint64(0)
	var titlePtrs []byte
	if cfg.legacyTitleList {
		titlePtrPos = pathPtrPos + uint64(len(pathPtrs))
		titleOf := func(e tEntry) string {
			if e.title != "" {
				return e.title
			}
			return e.path
		}
		order := make([]int, len(entries))
		for i := range order {
			order[i] = i
		}
		sort.Slice(order, func(x, y int) bool {
			tx, ty := titleOf(entries[order[x]]), titleOf(entries[order[y]])
			if tx != ty {
				return tx < ty
			}
			return order[x] < order[y]
		})
		for _, i := range order {
			titlePtrs = u32(titlePtrs, uint32(i))
		}
	}

	clusterPtrPos := pathPtrPos + uint64(len(pathPtrs)) + uint64(len(titlePtrs))
	clusterStart := clusterPtrPos + 8 // 1 cluster

	// Cluster 0 (§3.4): info byte (compresión en bits 0–3, extended en el bit 4) y
	// tras él, TODO comprimido: N+1 offsets relativos al inicio del área de
	// offsets (u32, o u64 si extended) y luego los blobs.
	osz := uint64(4)
	if extended {
		osz = 8
	}
	var area []byte
	off := osz * uint64(len(blobs)+1)
	putOff := func(v uint64) {
		if extended {
			area = u64(area, v)
		} else {
			area = u32(area, uint32(v))
		}
	}
	putOff(off)
	for _, b := range blobs {
		off += uint64(len(b))
		putOff(off)
	}
	for _, b := range blobs {
		area = append(area, b...)
	}

	info := compression
	if extended {
		info |= extendedFlag
	}
	cluster := []byte{info}
	switch compression {
	case compZstd:
		var cbuf bytes.Buffer
		zw, err := zstd.NewWriter(&cbuf)
		if err != nil {
			t.Fatal(err)
		}
		zw.Write(area)
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		cluster = append(cluster, cbuf.Bytes()...)
	case compXZ:
		var cbuf bytes.Buffer
		xw, err := xz.NewWriter(&cbuf)
		if err != nil {
			t.Fatal(err)
		}
		xw.Write(area)
		if err := xw.Close(); err != nil {
			t.Fatal(err)
		}
		cluster = append(cluster, cbuf.Bytes()...)
	default: // compNone — y también las compresiones inválidas de los tests
		cluster = append(cluster, area...)
	}

	checksumPos := clusterStart + uint64(len(cluster))

	// Header (§3.1). El UUID es ÚNICO por ZIM construido: la caché de clusters es
	// global con clave (uuid, cluster) — dos ZIMs de test con el mismo UUID se
	// servirían los clusters cacheados del otro.
	uuid := testUUID
	binary.LittleEndian.PutUint64(uuid[8:], uuidSeq.Add(1))

	var h []byte
	h = u32(h, magicZIM)
	h = u16(h, 6) // major
	h = u16(h, 1) // minor
	h = append(h, uuid[:]...)
	h = u32(h, uint32(len(entries)))
	h = u32(h, 1) // clusterCount
	h = u64(h, pathPtrPos)
	h = u64(h, titlePtrPos) // 0 salvo withLegacyTitleList (libzim moderno no la escribe, §20)
	h = u64(h, clusterPtrPos)
	h = u64(h, headerSize) // mimeListPos
	h = u32(h, mainPage)
	h = u32(h, noMainPage) // layoutPage: obsoleto
	h = u64(h, checksumPos)
	if len(h) != headerSize {
		t.Fatalf("header de %d bytes, esperado %d", len(h), headerSize)
	}

	data := make([]byte, 0, int(checksumPos)+checksumSize)
	data = append(data, h...)
	data = append(data, mimeList...)
	data = append(data, dirents...)
	data = append(data, pathPtrs...)
	data = append(data, titlePtrs...)
	data = u64(data, clusterStart)
	data = append(data, cluster...)
	sum := md5.Sum(data)
	data = append(data, sum[:]...)
	return data
}

var (
	testUUID = [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	uuidSeq  atomic.Uint64
)

// uuidOf extrae el UUID de unos bytes de ZIM construidos (header, offset 8).
func uuidOf(data []byte) (u [16]byte) {
	copy(u[:], data[8:24])
	return u
}

// openZIMBytes escribe los bytes a un fichero temporal y lo abre con el motor.
func openZIMBytes(t testing.TB, data []byte, opts *Options) (Archive, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "test.zim")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := Open(context.Background(), p, opts)
	if a != nil {
		t.Cleanup(func() { a.Close() })
	}
	return a, err
}

// exampleZIM: el archivo de referencia de los tests — 3 "artículos" (portada, otro
// artículo y una imagen) + metadata + portada W/mainPage + listing v1, calcado a la
// forma del ZIM File Example oficial (3 artículos, ns modernos, un cluster).
func exampleZIM(t testing.TB) []byte {
	t.Helper()
	return buildZIMC(t,
		[]string{"text/html", "image/png", "text/plain", "application/octet-stream"},
		[]tEntry{
			{ns: 'C', path: "favicon.png", mime: 1, content: []byte("PNGDATA")},
			{ns: 'C', path: "index.html", title: "Main Page", mime: 0, content: []byte("<html>main</html>")},
			{ns: 'C', path: "other.html", title: "Otro artículo", mime: 0, content: []byte("<html>otro</html>")},
			{ns: 'M', path: "Title", mime: 2, content: []byte("Test ZIM")},
			{ns: 'W', path: "mainPage", isRedirect: true, redirect: 1}, // → C/index.html
			{ns: 'X', path: "listing/titleOrdered/v1", mime: 3, content: []byte{1, 0, 0, 0}},
		},
		noMainPage, compNone, false) // la portada se declara vía W/mainPage, como los ZIM modernos
}
