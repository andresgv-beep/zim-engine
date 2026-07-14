package zim

import (
	"encoding/binary"
	"fmt"
)

// Header ZIM (§3.1): 80 bytes fijos en el offset 0, todo little-endian. Aquí se
// parsea y se valida con desconfianza total: ningún campo del fichero se usa sin
// comprobar que no desborda, que apunta dentro del archivo y que es coherente con
// el resto. Un header malo → error legible, jamás panic ni make() gigante.

const (
	headerSize = 80
	// magic ZIM: u32 LE en offset 0 = 72173914 (0x044D495A). En disco: 5A 49 4D 04.
	magicZIM = 72173914
	// mainPage = 0xFFFFFFFF ⇒ el archivo no declara portada en el header.
	noMainPage = 0xFFFFFFFF
	// Versiones major validadas (§12): 5 (viejo) y 6 (actual). minor es informativo.
	minMajorVersion = 5
	maxMajorVersion = 6
	// MD5 interno al final del fichero (§3.7).
	checksumSize = 16
)

type header struct {
	magic         uint32
	majorVersion  uint16
	minorVersion  uint16 // informativo, NO autoritativo (§13)
	uuid          [16]byte
	entryCount    uint32
	clusterCount  uint32
	pathPtrPos    uint64
	titlePtrPos   uint64 // LEGACY: libzim 9.3 ya no la escribe; validar antes de usar (§20)
	clusterPtrPos uint64
	mimeListPos   uint64
	mainPage      uint32
	layoutPage    uint32 // obsoleto; se parsea y se ignora
	checksumPos   uint64 // → MD5; también = fin de los datos
}

func parseHeader(b []byte) header {
	le := binary.LittleEndian
	h := header{
		magic:         le.Uint32(b[0:4]),
		majorVersion:  le.Uint16(b[4:6]),
		minorVersion:  le.Uint16(b[6:8]),
		entryCount:    le.Uint32(b[24:28]),
		clusterCount:  le.Uint32(b[28:32]),
		pathPtrPos:    le.Uint64(b[32:40]),
		titlePtrPos:   le.Uint64(b[40:48]),
		clusterPtrPos: le.Uint64(b[48:56]),
		mimeListPos:   le.Uint64(b[56:64]),
		mainPage:      le.Uint32(b[64:68]),
		layoutPage:    le.Uint32(b[68:72]),
		checksumPos:   le.Uint64(b[72:80]),
	}
	copy(h.uuid[:], b[8:24])
	return h
}

// validate: sanity completa del header contra el tamaño real del fichero (§16).
// Regla: todo offset vive en [headerSize, checksumPos) y toda multiplicación se
// comprueba SIN desbordar antes de usarse.
func (h *header) validate(fileSize int64) error {
	if h.magic != magicZIM {
		return fmt.Errorf("%w: bad magic 0x%08X (esperado 0x%08X)", ErrCorrupt, h.magic, uint32(magicZIM))
	}
	if h.majorVersion < minMajorVersion || h.majorVersion > maxMajorVersion {
		return fmt.Errorf("%w: major version %d (soportadas %d–%d)",
			ErrUnsupportedVersion, h.majorVersion, minMajorVersion, maxMajorVersion)
	}
	if fileSize < headerSize+checksumSize {
		return fmt.Errorf("%w: fichero de %d bytes, imposible para un ZIM", ErrCorrupt, fileSize)
	}
	// checksumPos marca el fin de los datos Y el inicio del MD5: tiene que ser
	// exactamente filesize−16 (§3.7). Cualquier otra cosa = truncado o basura.
	if h.checksumPos != uint64(fileSize)-checksumSize {
		return fmt.Errorf("%w: checksumPos=%d, esperado %d (filesize−16)",
			ErrCorrupt, h.checksumPos, uint64(fileSize)-checksumSize)
	}
	end := h.checksumPos

	// Cada sección apunta dentro de [headerSize, end).
	for _, s := range []struct {
		name string
		pos  uint64
	}{
		{"mimeListPos", h.mimeListPos},
		{"pathPtrPos", h.pathPtrPos},
		{"clusterPtrPos", h.clusterPtrPos},
	} {
		if s.pos < headerSize || s.pos >= end {
			return fmt.Errorf("%w: %s=%d fuera de [%d, %d)", ErrCorrupt, s.name, s.pos, headerSize, end)
		}
	}

	// Las listas de punteros caben antes del checksum. La comparación va con
	// divisiones para que un entryCount hostil no desborde la multiplicación.
	if uint64(h.entryCount) > (end-h.pathPtrPos)/8 {
		return fmt.Errorf("%w: path pointer list (%d entradas) no cabe en el fichero", ErrCorrupt, h.entryCount)
	}
	if uint64(h.clusterCount) > (end-h.clusterPtrPos)/8 {
		return fmt.Errorf("%w: cluster pointer list (%d clusters) no cabe en el fichero", ErrCorrupt, h.clusterCount)
	}

	if h.mainPage != noMainPage && h.mainPage >= h.entryCount {
		return fmt.Errorf("%w: mainPage=%d con entryCount=%d", ErrCorrupt, h.mainPage, h.entryCount)
	}
	// titlePtrPos NO se valida aquí: es legacy, puede venir a 0 o inválida sin que
	// el archivo esté roto (§20). Se valida solo si la Fase B llega a necesitarla.
	return nil
}
