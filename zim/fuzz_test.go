package zim

// Fuzzing (§9.3): un ZIM roto o malicioso JAMÁS tira el shim — error tipado, nunca
// panic, nunca OOM. El fuzzer trabaja in-memory (bytes.Reader vía newArchive) para
// no pagar un fichero temporal por ejecución.
//
//	go test -fuzz=FuzzOpen -fuzztime=60s ./zim/
//
// Sin -fuzz, las semillas corren como tests normales (regresión del corpus).

import (
	"bytes"
	"context"
	"io"
	"testing"
)

// fuzzLimits: límites pequeños para que el fuzzer no queme tiempo descomprimiendo
// gigas — el objetivo es cobertura de caminos, no throughput.
func fuzzLimits() Limits {
	l := DefaultLimits()
	l.MaxMimeListBytes = 1 << 20
	l.MaxEntryStringBytes = 64 << 10
	l.MaxClusterCompressedMB = 4
	l.MaxDecompressedClusterMB = 4
	l.MaxCachedClusterMB = 1
	l.MaxBlobMB = 4
	return l
}

func FuzzOpen(f *testing.F) {
	// Semillas: las variantes válidas del builder — el fuzzer muta desde ZIMs
	// correctos, que es como se llega a los caminos profundos del parser.
	f.Add(exampleZIM(f))
	entries := []tEntry{
		{ns: 'C', path: "a", mime: 0, content: []byte("contenido-a")},
		{ns: 'C', path: "b", title: "be", mime: 0, content: []byte("contenido-b")},
		{ns: 'W', path: "mainPage", isRedirect: true, redirect: 0},
	}
	mimes := []string{"text/plain"}
	f.Add(buildZIMC(f, mimes, entries, 0, compZstd, false))
	f.Add(buildZIMC(f, mimes, entries, 0, compZstd, true))
	f.Add(buildZIMC(f, mimes, entries, 0, compXZ, false))
	f.Add(buildZIMC(f, mimes, entries, 0, compNone, true))
	// Ciclo de redirects y truncados, para sembrar también los caminos de error.
	f.Add(buildZIM(f, mimes, []tEntry{
		{ns: 'W', path: "a", isRedirect: true, redirect: 1},
		{ns: 'W', path: "b", isRedirect: true, redirect: 0},
	}, 0))
	whole := exampleZIM(f)
	f.Add(whole[:headerSize])
	f.Add(whole[:len(whole)/2])

	f.Fuzz(func(t *testing.T, data []byte) {
		a, err := newArchive(bytes.NewReader(data), int64(len(data)), nil, fuzzLimits())
		if err != nil {
			return // rechazado limpiamente: eso es exactamente lo que se pide
		}
		defer a.Close()
		// Caché propia por ejecución: el uuid viene del input y colisionaría en
		// la global entre iteraciones del fuzzer.
		a.clusterCache = newLRUCache[clusterKey, *cachedCluster](1 << 20)
		exerciseArchive(a)
	})
}

// exerciseArchive recorre los caminos calientes del motor sobre un archive ya
// abierto. Los errores son resultados válidos; los panics no.
func exerciseArchive(a *archive) {
	ctx := context.Background()

	a.MainPage()
	a.Metadata("Title")
	a.EntryAtFullPath("C/index.html")
	a.Capabilities()

	n := a.hdr.entryCount
	if n > 128 {
		n = 128 // cobertura, no exhaustividad: el fuzzer itera millones de veces
	}
	for i := uint32(0); i < n; i++ {
		d, err := a.direntAtIndex(i)
		if err != nil {
			continue
		}
		e := &entry{a: a, idx: i, d: d}
		e.Title()
		e.FullPath()
		e.RedirectTarget()
		rc, _, err := e.Open(ctx)
		if err != nil {
			continue
		}
		io.CopyN(io.Discard, rc, 64<<10)
		rc.Close()
	}
}
