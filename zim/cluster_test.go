package zim

// Tests del paso 3 (clusters): contenido real por las tres compresiones soportadas,
// offsets normales y extended, estrategias S/C, cancelación por contexto y las
// filas de clusters de la matriz defensiva §25.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

// readEntry: abre una entrada por full path y devuelve sus bytes + BlobInfo.
func readEntry(t *testing.T, a Archive, full string) ([]byte, BlobInfo) {
	t.Helper()
	e, err := a.EntryAtFullPath(full)
	if err != nil {
		t.Fatal(err)
	}
	rc, info, err := e.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(data)) != info.Size {
		t.Fatalf("%s: leídos %d bytes, BlobInfo.Size=%d", full, len(data), info.Size)
	}
	return data, info
}

func TestBlobContent(t *testing.T) {
	cases := []struct {
		name        string
		compression byte
		extended    bool
	}{
		{"sin compresión", compNone, false},
		{"sin compresión extended", compNone, true},
		{"zstd", compZstd, false},
		{"zstd extended", compZstd, true},
		{"xz", compXZ, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := buildZIMC(t,
				[]string{"text/html", "image/png", "text/plain", "application/octet-stream"},
				[]tEntry{
					{ns: 'C', path: "favicon.png", mime: 1, content: []byte("PNGDATA")},
					{ns: 'C', path: "index.html", title: "Main Page", mime: 0, content: []byte("<html>main</html>")},
					{ns: 'C', path: "other.html", title: "Otro artículo", mime: 0, content: []byte("<html>otro</html>")},
					{ns: 'M', path: "Title", mime: 2, content: []byte("Test ZIM")},
					{ns: 'W', path: "mainPage", isRedirect: true, redirect: 1},
				},
				noMainPage, tc.compression, tc.extended)
			a, err := openZIMBytes(t, data, nil)
			if err != nil {
				t.Fatal(err)
			}

			// Blob del medio y primer blob (bstart = off0 es un caso aparte).
			html, info := readEntry(t, a, "C/index.html")
			if string(html) != "<html>main</html>" {
				t.Errorf("index.html = %q", html)
			}
			wantSeekable := tc.compression == compNone
			if info.Seekable != wantSeekable || info.Compressed == wantSeekable {
				t.Errorf("BlobInfo: seekable=%v compressed=%v (compresión %d)",
					info.Seekable, info.Compressed, tc.compression)
			}
			if info.MIME != "text/html" {
				t.Errorf("MIME = %q", info.MIME)
			}
			if fav, _ := readEntry(t, a, "C/favicon.png"); string(fav) != "PNGDATA" {
				t.Errorf("favicon = %q", fav)
			}
			if last, _ := readEntry(t, a, "C/other.html"); string(last) != "<html>otro</html>" {
				t.Errorf("other.html = %q", last)
			}

			// Metadata pasa por el mismo camino de blobs.
			title, err := a.Metadata("Title")
			if err != nil || title != "Test ZIM" {
				t.Errorf("Metadata(Title) = %q, %v", title, err)
			}
			// Abrir un redirect resuelve hasta el contenido final.
			body, _ := readEntry(t, a, "W/mainPage")
			if string(body) != "<html>main</html>" {
				t.Errorf("W/mainPage abierto = %q", body)
			}
		})
	}
}

func TestStreamingStrategy(t *testing.T) {
	// MaxCachedClusterMB = 0 fuerza la estrategia S en todo cluster comprimido:
	// misma semántica, camino de descarte + streaming.
	big := bytes.Repeat([]byte("relleno-"), 512) // 4 KiB por delante del blob objetivo
	data := buildZIMC(t, []string{"text/plain"}, []tEntry{
		{ns: 'C', path: "a", mime: 0, content: big},
		{ns: 'C', path: "b", mime: 0, content: []byte("objetivo")},
		{ns: 'C', path: "c", mime: 0, content: big},
	}, noMainPage, compZstd, false)

	l := DefaultLimits()
	l.MaxCachedClusterMB = 0
	a, err := openZIMBytes(t, data, &Options{Limits: &l})
	if err != nil {
		t.Fatal(err)
	}
	got, info := readEntry(t, a, "C/b")
	if string(got) != "objetivo" || info.Compressed != true {
		t.Errorf("estrategia S: %q (compressed=%v)", got, info.Compressed)
	}

	// Cerrar sin leer no puede romper nada (corta el decoder a medias).
	e, err := a.EntryAtFullPath("C/a")
	if err != nil {
		t.Fatal(err)
	}
	rc, _, err := e.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := rc.Close(); err != nil {
		t.Errorf("Close sin leer: %v", err)
	}
}

func TestCancellation(t *testing.T) {
	data := buildZIMC(t, []string{"text/html"}, []tEntry{
		{ns: 'C', path: "index.html", mime: 0, content: []byte("<html>main</html>")},
	}, noMainPage, compZstd, false)
	a, err := openZIMBytes(t, data, nil)
	if err != nil {
		t.Fatal(err)
	}
	e, err := a.EntryAtFullPath("C/index.html")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := e.Open(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Open con ctx cancelado: err = %v", err)
	}
}

func TestUnsupportedCompression(t *testing.T) {
	for _, comp := range []byte{compZlib, compBzip2, 9} {
		data := buildZIMC(t, []string{"text/html"}, []tEntry{
			{ns: 'C', path: "a", mime: 0, content: []byte("x")},
		}, noMainPage, comp, false)
		a, err := openZIMBytes(t, data, nil)
		if err != nil {
			t.Fatal(err)
		}
		e, err := a.EntryAtFullPath("C/a")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := e.Open(context.Background()); !errors.Is(err, ErrUnsupportedCompression) {
			t.Errorf("compresión %d: err = %v, esperado ErrUnsupportedCompression", comp, err)
		}
	}
}

func TestDecompressionBomb(t *testing.T) {
	// 1 MiB de ceros comprime a casi nada → el ratio §16 (default 200:1) tiene que
	// saltar ANTES de materializar nada.
	data := buildZIMC(t, []string{"application/octet-stream"}, []tEntry{
		{ns: 'C', path: "bomba", mime: 0, content: make([]byte, 1<<20)},
	}, noMainPage, compZstd, false)
	a, err := openZIMBytes(t, data, nil)
	if err != nil {
		t.Fatal(err)
	}
	e, err := a.EntryAtFullPath("C/bomba")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Open(context.Background()); !errors.Is(err, ErrResourceLimit) {
		t.Errorf("bomba: err = %v, esperado ErrResourceLimit", err)
	}
}

func TestBlobOutOfRange(t *testing.T) {
	// El dirent apunta a un blob que el cluster no tiene → ErrCorrupt, no panic.
	data := buildZIM(t, []string{"text/html"}, []tEntry{
		{ns: 'C', path: "a", mime: 0, content: []byte("x"), blobAdd: 5},
	}, noMainPage)
	a, err := openZIMBytes(t, data, nil)
	if err != nil {
		t.Fatal(err)
	}
	e, err := a.EntryAtFullPath("C/a")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Open(context.Background()); !errors.Is(err, ErrCorrupt) {
		t.Errorf("blob fuera de rango: err = %v, esperado ErrCorrupt", err)
	}
}

func TestCloseWaitsForReaders(t *testing.T) {
	// §23: Close() con un reader abierto espera a que se cierre; mientras tanto el
	// reader sigue leyendo bytes válidos.
	a, err := openZIMBytes(t, exampleZIM(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	e, err := a.EntryAtFullPath("C/index.html")
	if err != nil {
		t.Fatal(err)
	}
	rc, _, err := e.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	closed := make(chan struct{})
	go func() { a.Close(); close(closed) }()

	// Dar tiempo a que la goroutine llegue de verdad a Close() antes de comprobar
	// que sigue bloqueada.
	time.Sleep(20 * time.Millisecond)
	select {
	case <-closed:
		t.Fatal("Close() no esperó al reader activo")
	default:
	}
	if data, err := io.ReadAll(rc); err != nil || string(data) != "<html>main</html>" {
		t.Errorf("lectura con Close pendiente: %q, %v", data, err)
	}
	rc.Close()
	<-closed // ahora sí: el último release desbloquea Close()

	if _, err := a.EntryAtFullPath("C/index.html"); !errors.Is(err, ErrClosed) {
		t.Errorf("tras Close: err = %v", err)
	}
}
