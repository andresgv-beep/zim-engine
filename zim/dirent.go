package zim

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Dirents (§3.3): tamaño variable, se llega a ellos por la path pointer list. Dos
// formas — content (16 bytes fijos + strings) y redirect (12 bytes fijos + strings);
// linktarget (0xFFFE) y deleted (0xFFFD) tienen la forma del redirect y se ignoran.
// Tras los strings viene el "parameter" (len en el byte 2), que se salta.

type direntKind uint8

const (
	direntContent direntKind = iota
	direntRedirect
	direntLinkTarget // 0xFFFE: se parsea para no romper, no se sirve
	direntDeleted    // 0xFFFD: ídem
)

// Valores especiales del campo mimetype.
const (
	mimeRedirect   = 0xFFFF
	mimeLinkTarget = 0xFFFE
	mimeDeleted    = 0xFFFD
)

type dirent struct {
	kind      direntKind
	mimeIndex uint16 // índice en la MIME list (solo content)
	namespace byte
	cluster   uint32 // solo content
	blob      uint32 // solo content
	redirect  uint32 // índice de dirent destino (solo redirect)
	path      string
	title     string // ya resuelto: title vacío en disco ⇒ title = path
}

func (d *dirent) key() EntryKey { return EntryKey{Namespace: d.namespace, Path: d.path} }

// errShortDirent: sentinela interno — el buffer no contiene el dirent completo y
// el fichero aún tiene bytes detrás. El llamante relee con más ventana.
var errShortDirent = errors.New("zim: dirent incompleto en el buffer")

// parseDirent parsea el dirent que EMPIEZA en b[0], con validación defensiva
// completa (§16). hasMore indica si el fichero tiene más datos después de b (si el
// dirent no cabe y hasMore, se pide más con errShortDirent; si no, es corrupción).
func (a *archive) parseDirent(b []byte, hasMore bool) (dirent, error) {
	short := func() (dirent, error) {
		if hasMore {
			return dirent{}, errShortDirent
		}
		return dirent{}, fmt.Errorf("%w: dirent truncado al final de los datos", ErrCorrupt)
	}
	if len(b) < 12 {
		return short()
	}
	le := binary.LittleEndian

	d := dirent{
		mimeIndex: le.Uint16(b[0:2]),
		namespace: b[3],
	}
	if paramLen := int64(b[2]); paramLen > a.limits.MaxParameterBytes {
		return dirent{}, fmt.Errorf("%w: dirent parameter de %d bytes", ErrResourceLimit, paramLen)
	}

	var stringsOff int
	switch d.mimeIndex {
	case mimeRedirect:
		d.kind = direntRedirect
		d.redirect = le.Uint32(b[8:12])
		if d.redirect >= a.hdr.entryCount {
			return dirent{}, fmt.Errorf("%w: redirect a dirent %d con entryCount=%d",
				ErrCorrupt, d.redirect, a.hdr.entryCount)
		}
		stringsOff = 12
	case mimeLinkTarget, mimeDeleted:
		if d.mimeIndex == mimeLinkTarget {
			d.kind = direntLinkTarget
		} else {
			d.kind = direntDeleted
		}
		stringsOff = 12
	default:
		d.kind = direntContent
		if int(d.mimeIndex) >= len(a.mimes) {
			return dirent{}, fmt.Errorf("%w: mimetype index %d con %d MIME types",
				ErrCorrupt, d.mimeIndex, len(a.mimes))
		}
		if len(b) < 16 {
			return short()
		}
		d.cluster = le.Uint32(b[8:12])
		d.blob = le.Uint32(b[12:16])
		if d.cluster >= a.hdr.clusterCount {
			return dirent{}, fmt.Errorf("%w: cluster %d con clusterCount=%d",
				ErrCorrupt, d.cluster, a.hdr.clusterCount)
		}
		// blob se valida contra el nº real de blobs del cluster al abrirlo:
		// aquí aún no se ha descomprimido nada y no se conoce ese contador.
		stringsOff = 16
	}

	path, next, err := a.parseCString(b, stringsOff, hasMore)
	if err != nil {
		return dirent{}, err
	}
	title, _, err := a.parseCString(b, next, hasMore)
	if err != nil {
		return dirent{}, err
	}
	d.path = path
	if title == "" {
		title = path // regla de la spec: title vacío ⇒ title = path
	}
	d.title = title
	return d, nil
}

// parseCString extrae el string \0-terminado que empieza en b[off], acotado por
// §16. Devuelve el string y el offset del byte siguiente al terminador.
func (a *archive) parseCString(b []byte, off int, hasMore bool) (string, int, error) {
	max := a.limits.MaxEntryStringBytes
	end := len(b)
	if lim := int64(off) + max + 1; lim < int64(end) {
		end = int(lim)
	}
	for i := off; i < end; i++ {
		if b[i] == 0 {
			return string(b[off:i]), i + 1, nil
		}
	}
	if int64(end-off) > max {
		return "", 0, fmt.Errorf("%w: string de dirent supera %d bytes", ErrResourceLimit, max)
	}
	if hasMore {
		return "", 0, errShortDirent
	}
	return "", 0, fmt.Errorf("%w: string de dirent sin terminador", ErrCorrupt)
}

// readDirentAt lee y parsea el dirent en off con lecturas progresivas: una sola
// ReadAt de 1 KiB cubre el 99% de los dirents (antes eran 3 syscalls por dirent);
// solo los raros con strings enormes releen con más ventana.
func (a *archive) readDirentAt(off int64) (dirent, error) {
	dataEnd := int64(a.hdr.checksumPos)
	if off < headerSize || off+12 > dataEnd {
		return dirent{}, fmt.Errorf("%w: dirent en offset %d fuera de rango", ErrCorrupt, off)
	}
	for size := int64(1024); ; size *= 8 {
		if size > dataEnd-off {
			size = dataEnd - off
		}
		buf := make([]byte, size)
		if _, err := a.f.ReadAt(buf, off); err != nil {
			return dirent{}, fmt.Errorf("%w: leyendo dirent en %d: %v", ErrCorrupt, off, err)
		}
		d, err := a.parseDirent(buf, off+size < dataEnd)
		if !errors.Is(err, errShortDirent) {
			return d, err
		}
	}
}
