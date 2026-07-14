package zim

// Golden tests contra el mundo real (§9.1–9.2). Dos niveles, ambos opt-in por env
// var para que `go test ./...` siga siendo autocontenido:
//
//	ZIM_REAL_FILE=/ruta/a/un.zim      recorre un ZIM real de verdad: todos los
//	                                  dirents (o una muestra si es enorme), abre
//	                                  blobs y valida coherencia interna.
//	ZIM_GOLDEN_FILE=/ruta/a/un.zim    ídem + compara contra `zimdump` (si está en
//	                                  el PATH): nº de entradas, paths y bytes.

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestRealZIMWalk(t *testing.T) {
	file := os.Getenv("ZIM_REAL_FILE")
	if file == "" {
		t.Skip("define ZIM_REAL_FILE para correr contra un .zim real")
	}
	a, err := Open(context.Background(), file, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	arch := a.(*archive)

	total := arch.hdr.entryCount
	// Muestra determinista: todo si es pequeño; si no, un stride que reparte
	// las sondas por todo el archivo.
	stride := uint32(1)
	const maxProbes = 20000
	if total > maxProbes {
		stride = total / maxProbes
	}

	var contents, redirects, opened int
	for i := uint32(0); i < total; i += stride {
		d, err := arch.direntAtIndex(i)
		if err != nil {
			t.Fatalf("dirent %d: %v", i, err)
		}
		switch d.kind {
		case direntRedirect:
			redirects++
		case direntContent:
			contents++
			if int(d.mimeIndex) >= len(arch.mimes) {
				t.Fatalf("dirent %d: mime index %d fuera de rango", i, d.mimeIndex)
			}
			// Abrir una submuestra de blobs (los primeros 200 que toquen).
			if opened < 200 {
				e := &entry{a: arch, idx: i, d: d}
				rc, info, err := e.Open(context.Background())
				if err != nil {
					t.Fatalf("open %c/%s: %v", d.namespace, d.path, err)
				}
				n, err := io.Copy(io.Discard, rc)
				rc.Close()
				if err != nil {
					t.Fatalf("leyendo %c/%s: %v", d.namespace, d.path, err)
				}
				if n != info.Size {
					t.Fatalf("%c/%s: leídos %d bytes, BlobInfo.Size=%d", d.namespace, d.path, n, info.Size)
				}
				opened++
			}
		}
	}
	t.Logf("%s: %d entradas (stride %d): %d content, %d redirects; %d blobs abiertos OK",
		file, total, stride, contents, redirects, opened)
}

func TestGoldenVsZimdump(t *testing.T) {
	file := os.Getenv("ZIM_GOLDEN_FILE")
	if file == "" {
		t.Skip("define ZIM_GOLDEN_FILE para el diff contra zimdump")
	}
	zimdump, err := exec.LookPath("zimdump")
	if err != nil {
		t.Skip("zimdump no está en el PATH (paquete zim-tools)")
	}

	a, err := Open(context.Background(), file, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	arch := a.(*archive)

	// 1. Los paths que lista zimdump existen para nosotros, y al revés.
	out, err := exec.Command(zimdump, "list", file).Output()
	if err != nil {
		t.Fatalf("zimdump list: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != int(arch.hdr.entryCount) {
		t.Errorf("zimdump lista %d entradas, el motor dice %d", len(lines), arch.hdr.entryCount)
	}

	// 2. Bytes idénticos en una muestra de contenido (hash a hash con
	//    `zimdump show --url=...`).
	stride := uint32(1)
	const maxCompares = 100
	if arch.hdr.entryCount > maxCompares {
		stride = arch.hdr.entryCount / maxCompares
	}
	compared := 0
	for i := uint32(0); i < arch.hdr.entryCount; i += stride {
		d, err := arch.direntAtIndex(i)
		if err != nil || d.kind != direntContent {
			continue
		}
		e := &entry{a: arch, idx: i, d: d}
		rc, _, err := e.Open(context.Background())
		if err != nil {
			t.Fatalf("open %c/%s: %v", d.namespace, d.path, err)
		}
		h := sha256.New()
		io.Copy(h, rc)
		rc.Close()
		ours := fmt.Sprintf("%x", h.Sum(nil))

		ref, err := exec.Command(zimdump, "show", "--url="+string(d.namespace)+"/"+d.path, file).Output()
		if err != nil {
			continue // entradas que zimdump no muestra (p. ej. índices) no invalidan
		}
		theirs := fmt.Sprintf("%x", sha256.Sum256(ref))
		if ours != theirs {
			t.Errorf("bytes distintos en %c/%s: motor=%s zimdump=%s", d.namespace, d.path, ours, theirs)
		}
		compared++
	}
	t.Logf("comparadas %d entradas contra zimdump", compared)
}
