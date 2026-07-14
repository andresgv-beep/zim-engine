package zim

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// Path index (§3.3): los dirents están ordenados por (namespace, path) y la path
// pointer list da el offset de cada uno → búsqueda binaria O(log n). Sin caché
// todavía (paso 4): cada sonda lee su puntero y su dirent con ReadAt puro, que es
// exactamente el modo bajo-en-RAM previsto para la Pi (ZIM_PTRLIST_IN_RAM=0).

// compareKey: orden de la spec — namespace primero, luego path byte a byte.
func compareKey(a, b EntryKey) int {
	if a.Namespace != b.Namespace {
		if a.Namespace < b.Namespace {
			return -1
		}
		return 1
	}
	return strings.Compare(a.Path, b.Path)
}

// pathPtrAt lee el puntero i de la path pointer list y lo valida.
func (a *archive) pathPtrAt(i uint32) (int64, error) {
	if i >= a.hdr.entryCount {
		return 0, fmt.Errorf("%w: dirent index %d con entryCount=%d", ErrCorrupt, i, a.hdr.entryCount)
	}
	var buf [8]byte
	if _, err := a.f.ReadAt(buf[:], int64(a.hdr.pathPtrPos)+8*int64(i)); err != nil {
		return 0, fmt.Errorf("%w: leyendo path pointer %d: %v", ErrCorrupt, i, err)
	}
	ptr := binary.LittleEndian.Uint64(buf[:])
	if ptr < headerSize || ptr >= a.hdr.checksumPos {
		return 0, fmt.Errorf("%w: path pointer %d → %d fuera de rango", ErrCorrupt, i, ptr)
	}
	return int64(ptr), nil
}

// direntAtIndex: puntero i → dirent parseado, con caché LRU (§4) — la búsqueda
// binaria machaca los mismos pivotes en cada lookup. El dirent es inmutable
// (strings + escalares), compartirlo es seguro.
func (a *archive) direntAtIndex(i uint32) (dirent, error) {
	if d, ok := a.direntCache.get(i); ok {
		return d, nil
	}
	ptr, err := a.pathPtrAt(i)
	if err != nil {
		return dirent{}, err
	}
	d, err := a.readDirentAt(ptr)
	if err != nil {
		return dirent{}, err
	}
	a.direntCache.put(i, d, 1)
	return d, nil
}

// findByKey: búsqueda binaria sobre la path pointer list. Devuelve el índice y el
// dirent, o ErrNotFound. Si el archivo miente (orden roto), la búsqueda puede no
// encontrar una clave existente — eso es un archivo corrupto comportándose como
// tal, nunca un panic.
func (a *archive) findByKey(key EntryKey) (uint32, dirent, error) {
	lo, hi := uint32(0), a.hdr.entryCount
	for lo < hi {
		mid := lo + (hi-lo)/2
		d, err := a.direntAtIndex(mid)
		if err != nil {
			return 0, dirent{}, err
		}
		switch c := compareKey(key, d.key()); {
		case c == 0:
			return mid, d, nil
		case c < 0:
			hi = mid
		default:
			lo = mid + 1
		}
	}
	return 0, dirent{}, fmt.Errorf("%w: %c/%s", ErrNotFound, key.Namespace, key.Path)
}

// followRedirects resuelve una cadena de redirects con tope §16. Devuelve el
// dirent final (content) o ErrRedirectCycle si la cadena no muere a tiempo.
func (a *archive) followRedirects(idx uint32, d dirent) (uint32, dirent, error) {
	for depth := 0; depth < a.limits.MaxRedirectDepth; depth++ {
		if d.kind != direntRedirect {
			return idx, d, nil
		}
		next, err := a.direntAtIndex(d.redirect)
		if err != nil {
			return 0, dirent{}, err
		}
		idx, d = d.redirect, next
	}
	return 0, dirent{}, fmt.Errorf("%w: profundidad > %d", ErrRedirectCycle, a.limits.MaxRedirectDepth)
}
