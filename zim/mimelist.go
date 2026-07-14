package zim

import (
	"fmt"
	"io"
)

// MIME list (§3.2): strings UTF-8 \0-terminados a partir de mimeListPos; la lista
// termina con un string vacío (dos \0 seguidos). Se lee UNA vez al abrir. El índice
// de cada string es el que referencian los dirents (campo mimetype).

// readMimeList lee y parsea la lista. dataEnd (= checksumPos) acota la lectura: si
// el terminador no aparece antes de dataEnd o del límite §16, el archivo está roto
// o es hostil — error, no lectura infinita.
func readMimeList(r io.ReaderAt, pos int64, dataEnd int64, limits Limits) ([]string, error) {
	max := limits.MaxMimeListBytes
	if avail := dataEnd - pos; avail < max {
		max = avail
	}

	var (
		mimes []string
		data  []byte
		start int // inicio del string en curso dentro de data
	)
	for {
		// Chunk siguiente, acotado por el límite.
		if int64(len(data)) >= max {
			return nil, fmt.Errorf("%w: MIME list sin terminador tras %d bytes", ErrResourceLimit, len(data))
		}
		n := int64(4096)
		if rem := max - int64(len(data)); rem < n {
			n = rem
		}
		chunk := make([]byte, n)
		read, err := r.ReadAt(chunk, pos+int64(len(data)))
		if read == 0 && err != nil {
			return nil, fmt.Errorf("%w: leyendo MIME list: %v", ErrCorrupt, err)
		}
		prev := len(data)
		data = append(data, chunk[:read]...)

		// Consumir los strings completos que hayan aparecido.
		for i := prev; i < len(data); i++ {
			if data[i] != 0 {
				continue
			}
			if i == start { // string vacío ⇒ fin de la lista
				return mimes, nil
			}
			mimes = append(mimes, string(data[start:i]))
			start = i + 1
		}
		if err != nil { // EOF/corto sin haber visto el terminador
			return nil, fmt.Errorf("%w: MIME list sin terminador (EOF)", ErrCorrupt)
		}
	}
}
