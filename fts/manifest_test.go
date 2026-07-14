package fts

import (
	"strings"
	"testing"

	"github.com/andresgv-beep/zim-engine/zim"
)

// fakeArchive: solo UUID y EntryCount, que es lo único que Matches consulta. El
// resto de la interfaz embebida haría panic si se tocara — así el test también
// vigila que Matches no dependa de más superficie de la declarada.
type fakeArchive struct {
	zim.Archive
	uuid  [16]byte
	count uint32
}

func (f fakeArchive) UUID() [16]byte     { return f.uuid }
func (f fakeArchive) EntryCount() uint32 { return f.count }

func TestManifestMatches(t *testing.T) {
	a := fakeArchive{uuid: [16]byte{0xAA, 0xBB, 1, 2, 3}, count: 8606}
	m := newManifest(a, BuildOptions{Language: "spa"}, Tally{Candidates: 1400, Indexed: 1358, Skipped: 42})

	if err := m.Matches(a); err != nil {
		t.Fatalf("mismo ZIM debería casar: %v", err)
	}
	if m.Analyzer != "es" {
		t.Errorf("analizador para spa = %q, quiero es", m.Analyzer)
	}

	otro := fakeArchive{uuid: [16]byte{0x99}, count: 8606}
	if err := m.Matches(otro); err == nil || !strings.Contains(err.Error(), "otro ZIM") {
		t.Errorf("uuid distinto debería rechazarse con 'otro ZIM', fue: %v", err)
	}

	cambiado := fakeArchive{uuid: a.uuid, count: 9999}
	if err := m.Matches(cambiado); err == nil || !strings.Contains(err.Error(), "entryCount") {
		t.Errorf("entryCount distinto debería rechazarse, fue: %v", err)
	}

	viejo := m
	viejo.Schema = 0
	if err := viejo.Matches(a); err == nil || !strings.Contains(err.Error(), "esquema") {
		t.Errorf("esquema viejo debería exigir reindexar, fue: %v", err)
	}
}
