package fts

// El manifiesto hace del índice un ARTEFACTO distribuible: se construye en el PC
// y viaja al pool de la Pi como un fichero más, al lado del .zim. Sin él, un
// índice copiado no puede verificarse contra su ZIM y un día produce resultados
// fantasma imposibles de depurar. Además es el marcador de build completo: se
// escribe DESPUÉS del Close del Builder, así que su ausencia = build interrumpido
// o a medias → reindexar.

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/andresgv-beep/zim-engine/zim"
)

// ManifestName es el nombre del fichero de manifiesto dentro del dir del índice.
const ManifestName = "zim-fts.json"

// SchemaVersion versiona el esquema del documento indexado (campos + mapping).
// Si cambia extract.go o buildIndexMapping de forma incompatible, se incrementa:
// los índices con schema viejo se rechazan y se reindexan.
const SchemaVersion = 1

type Manifest struct {
	Schema     int    `json:"schema"`
	ZimUUID    string `json:"zimUUID"` // hex, 32 chars
	EntryCount uint32 `json:"entryCount"`

	// Completitud HONESTA (FTS-AUDIT BUG-2). El marcador "el manifiesto existe"
	// prueba que el proceso llegó al final; estos campos prueban que hizo su
	// trabajo. Un índice con Failed alto no debe escribirse (el build falla), y
	// un Indexed/Candidates bajo debe verse en amarillo en el Panel.
	Candidates int `json:"candidates"` // elegibles: NS artículo + HTML + no redirect
	Indexed    int `json:"indexed"`    // los que entraron de verdad al índice
	Skipped    int `json:"skipped"`    // sin texto útil (legítimo: vacías, solo-imagen)
	Failed     int `json:"failed"`     // errores reales de lectura/extracción

	Language  string `json:"language"`
	Analyzer  string `json:"analyzer"`
	StoreBody bool   `json:"storeBody"`
	BuiltAt   string `json:"builtAt"` // RFC3339 UTC
}

func writeManifest(dir string, m Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, ManifestName), append(data, '\n'), 0o644)
}

// ReadManifest lee el manifiesto de un índice. Error = índice sin manifiesto
// (build a medias, o de una versión anterior a este esquema): tratar como
// inexistente y reindexar.
func ReadManifest(dir string) (Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, ManifestName))
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("manifiesto corrupto: %w", err)
	}
	return m, nil
}

// Matches verifica que este índice corresponde EXACTAMENTE a ese archive y que
// este binario sabe leerlo. Es la comprobación que permite que un índice
// construido en el PC viaje a la Pi con garantías.
func (m Manifest) Matches(a zim.Archive) error {
	if m.Schema != SchemaVersion {
		return fmt.Errorf("esquema del índice v%d ≠ v%d del binario: reindexar", m.Schema, SchemaVersion)
	}
	if got := hex.EncodeToString(uuidBytes(a)); m.ZimUUID != got {
		return fmt.Errorf("el índice es de otro ZIM (uuid %.8s… ≠ %.8s…)", m.ZimUUID, got)
	}
	if m.EntryCount != a.EntryCount() {
		return fmt.Errorf("entryCount %d ≠ %d: el ZIM cambió bajo el índice", m.EntryCount, a.EntryCount())
	}
	return nil
}

func uuidBytes(a zim.Archive) []byte {
	u := a.UUID()
	return u[:]
}

// Tally: recuento del build, separando lo legítimo (Skipped) de lo que es un
// error real (Failed). Es lo que decide si el build merece manifiesto.
type Tally struct {
	Candidates int
	Indexed    int
	Skipped    int
	Failed     int
}

func newManifest(a zim.Archive, opts BuildOptions, t Tally) Manifest {
	return Manifest{
		Schema:     SchemaVersion,
		ZimUUID:    hex.EncodeToString(uuidBytes(a)),
		EntryCount: a.EntryCount(),
		Candidates: t.Candidates,
		Indexed:    t.Indexed,
		Skipped:    t.Skipped,
		Failed:     t.Failed,
		Language:   opts.Language,
		Analyzer:   analyzerFor(opts.Language),
		StoreBody:  opts.StoreBody,
		BuiltAt:    time.Now().UTC().Format(time.RFC3339),
	}
}
