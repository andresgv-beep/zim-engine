package zim

// Tests del paso 2 (header → mimelist → dirent → pathindex) + primeras filas de la
// matriz defensiva §25: archivo corrupto NUNCA panic, siempre error tipado legible.

import (
	"encoding/binary"
	"errors"
	"testing"
)

func TestOpenAndLookup(t *testing.T) {
	data := exampleZIM(t)
	a, err := openZIMBytes(t, data, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := a.EntryCount(); got != 6 {
		t.Errorf("EntryCount = %d, esperado 6", got)
	}
	if a.UUID() != uuidOf(data) {
		t.Errorf("UUID = %x, esperado %x", a.UUID(), uuidOf(data))
	}

	e, err := a.EntryAtFullPath("C/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if e.Title() != "Main Page" || e.MimeType() != "text/html" || e.IsRedirect() {
		t.Errorf("C/index.html: title=%q mime=%q redirect=%v", e.Title(), e.MimeType(), e.IsRedirect())
	}
	if e.FullPath() != "C/index.html" {
		t.Errorf("FullPath = %q", e.FullPath())
	}

	// Title vacío en disco ⇒ Title() = path (regla de la spec).
	fav, err := a.EntryAt(EntryKey{'C', "favicon.png"})
	if err != nil {
		t.Fatal(err)
	}
	if fav.Title() != "favicon.png" || fav.MimeType() != "image/png" {
		t.Errorf("favicon: title=%q mime=%q", fav.Title(), fav.MimeType())
	}

	if _, err := a.EntryAt(EntryKey{'C', "no-existe"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("lookup inexistente: err = %v, esperado ErrNotFound", err)
	}
	if _, err := a.EntryAtFullPath("sin-namespace"); !errors.Is(err, ErrNotFound) {
		t.Errorf("full path malformado: err = %v, esperado ErrNotFound", err)
	}
}

func TestRedirectAndMainPage(t *testing.T) {
	a, err := openZIMBytes(t, exampleZIM(t), nil)
	if err != nil {
		t.Fatal(err)
	}

	w, err := a.EntryAt(EntryKey{'W', "mainPage"})
	if err != nil {
		t.Fatal(err)
	}
	if !w.IsRedirect() || w.MimeType() != "" {
		t.Errorf("W/mainPage: redirect=%v mime=%q", w.IsRedirect(), w.MimeType())
	}
	tgt, ok := w.RedirectTarget()
	if !ok || tgt != (EntryKey{'C', "index.html"}) {
		t.Errorf("RedirectTarget = %v/%v", tgt, ok)
	}

	// MainPage resuelve el redirect W/mainPage hasta el contenido real.
	mp, err := a.MainPage()
	if err != nil {
		t.Fatal(err)
	}
	if mp.FullPath() != "C/index.html" || mp.IsRedirect() {
		t.Errorf("MainPage = %q (redirect=%v)", mp.FullPath(), mp.IsRedirect())
	}
}

func TestMainPageFromHeader(t *testing.T) {
	// Sin W/mainPage: la portada sale de header.mainPage (índice 1 = C/index.html).
	data := buildZIM(t, []string{"text/html"}, []tEntry{
		{ns: 'C', path: "a.html", mime: 0, content: []byte("a")},
		{ns: 'C', path: "index.html", title: "Portada", mime: 0, content: []byte("b")},
	}, 1)
	a, err := openZIMBytes(t, data, nil)
	if err != nil {
		t.Fatal(err)
	}
	mp, err := a.MainPage()
	if err != nil {
		t.Fatal(err)
	}
	if mp.FullPath() != "C/index.html" {
		t.Errorf("MainPage = %q", mp.FullPath())
	}

	// Sin W/mainPage NI header.mainPage → ErrNotFound, no inventarse portadas.
	data = buildZIM(t, []string{"text/html"}, []tEntry{
		{ns: 'C', path: "a.html", mime: 0, content: []byte("a")},
	}, noMainPage)
	a2, err := openZIMBytes(t, data, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a2.MainPage(); !errors.Is(err, ErrNotFound) {
		t.Errorf("MainPage sin portada: err = %v, esperado ErrNotFound", err)
	}
}

func TestRedirectCycle(t *testing.T) {
	// W/a → W/b → W/a: la resolución tiene que morir en ErrRedirectCycle (§16).
	data := buildZIM(t, []string{"text/html"}, []tEntry{
		{ns: 'W', path: "a", isRedirect: true, redirect: 1},
		{ns: 'W', path: "b", isRedirect: true, redirect: 0},
	}, 0)
	a, err := openZIMBytes(t, data, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.MainPage(); !errors.Is(err, ErrRedirectCycle) {
		t.Errorf("ciclo de redirects: err = %v, esperado ErrRedirectCycle", err)
	}
}

func TestCapabilities(t *testing.T) {
	a, err := openZIMBytes(t, exampleZIM(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	c := a.Capabilities()
	if !c.NewNamespaces || !c.HasMainPageEntry || !c.HasTitleListingV1 {
		t.Errorf("capacidades esperadas ausentes: %+v", c)
	}
	if c.HasTitleListingV0 || c.HasLegacyTitleList || c.HasFullTextXapian || c.HasExtendedCluster {
		t.Errorf("capacidades fantasma: %+v", c)
	}
}

// TestCorruptHeader: mutaciones sobre un ZIM válido → error tipado, jamás panic.
func TestCorruptHeader(t *testing.T) {
	le := binary.LittleEndian
	cases := []struct {
		name   string
		mutate func([]byte) []byte
		want   error
	}{
		{"magic roto", func(b []byte) []byte { le.PutUint32(b[0:4], 0xDEADBEEF); return b }, ErrCorrupt},
		{"major 7", func(b []byte) []byte { le.PutUint16(b[4:6], 7); return b }, ErrUnsupportedVersion},
		{"major 4", func(b []byte) []byte { le.PutUint16(b[4:6], 4); return b }, ErrUnsupportedVersion},
		{"truncado", func(b []byte) []byte { return b[:len(b)-10] }, ErrCorrupt},
		{"fichero enano", func(b []byte) []byte { return b[:20] }, ErrCorrupt},
		{"pathPtrPos fuera de rango", func(b []byte) []byte {
			le.PutUint64(b[32:40], uint64(len(b))+100)
			return b
		}, ErrCorrupt},
		{"mimeListPos dentro del header", func(b []byte) []byte {
			le.PutUint64(b[56:64], 10)
			return b
		}, ErrCorrupt},
		{"entryCount desorbitado", func(b []byte) []byte {
			le.PutUint32(b[24:28], 0xFFFFFFFF)
			return b
		}, ErrCorrupt},
		{"mainPage fuera de entryCount", func(b []byte) []byte {
			le.PutUint32(b[64:68], 6) // entryCount = 6 ⇒ índice 6 no existe
			return b
		}, ErrCorrupt},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := tc.mutate(exampleZIM(t))
			if _, err := openZIMBytes(t, data, nil); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, esperado %v", err, tc.want)
			}
		})
	}
}

func TestResourceLimits(t *testing.T) {
	t.Run("mime list sobre el límite", func(t *testing.T) {
		l := DefaultLimits()
		l.MaxMimeListBytes = 2 // "text/html…" no cabe → ErrResourceLimit
		_, err := openZIMBytes(t, exampleZIM(t), &Options{Limits: &l})
		if !errors.Is(err, ErrResourceLimit) {
			t.Errorf("err = %v, esperado ErrResourceLimit", err)
		}
	})
	t.Run("string de dirent sobre el límite", func(t *testing.T) {
		long := make([]byte, 100)
		for i := range long {
			long[i] = 'a'
		}
		data := buildZIM(t, []string{"text/html"}, []tEntry{
			{ns: 'C', path: string(long), mime: 0, content: []byte("x")},
		}, noMainPage)
		l := DefaultLimits()
		l.MaxEntryStringBytes = 10
		a, err := openZIMBytes(t, data, &Options{Limits: &l})
		if err != nil {
			t.Fatal(err) // abrir no toca dirents más allá de capacidades (que degradan)
		}
		_, err = a.EntryAt(EntryKey{'C', string(long)})
		if !errors.Is(err, ErrResourceLimit) {
			t.Errorf("err = %v, esperado ErrResourceLimit", err)
		}
	})
}

func TestClosed(t *testing.T) {
	a, err := openZIMBytes(t, exampleZIM(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil { // idempotente (§23)
		t.Errorf("segundo Close: err = %v", err)
	}
	if _, err := a.EntryAtFullPath("C/index.html"); !errors.Is(err, ErrClosed) {
		t.Errorf("EntryAt tras Close: err = %v, esperado ErrClosed", err)
	}
	if _, err := a.MainPage(); !errors.Is(err, ErrClosed) {
		t.Errorf("MainPage tras Close: err = %v, esperado ErrClosed", err)
	}
}
