package fts

import (
	"fmt"
	"strings"

	"github.com/andresgv-beep/zim-engine/zim"

	"github.com/blevesearch/bleve/v2"
)

// Hit: un resultado full-text. Path es el FullPath de la entrada ("C/Saturno"), que
// el shim recorta a la ruta pública. Snippet ya viene resaltado con <mark>…</mark>
// alrededor de los términos (el shim lo reutiliza o lo limpia, como con kiwix).
type Hit struct {
	Path    string
	Title   string
	Snippet string
	Score   float64
}

// Index: un índice bleve abierto para consulta. Seguro para uso concurrente (bleve
// serializa internamente sus lecturas).
type Index struct {
	idx      bleve.Index
	manifest Manifest
}

// Open abre un índice ya construido en dir y VERIFICA que corresponde a ese
// archive (FTS-AUDIT BUG-1). Es la cerradura del camino de apertura: escribir un
// manifiesto no es verificarlo, igual que esconder un botón no es un permiso.
// Sin esta comprobación, copiar el .bleve equivocado al pool sirve resultados
// fantasma sin una sola queja — y todo el diseño de índices distribuibles
// (INDEXER.md) depende de que esto no pueda pasar.
//
// Errores:
//   - manifiesto ausente  → build interrumpido o índice pre-manifiesto: reindexar
//   - Matches falla       → índice de otro ZIM, otro entryCount u otro esquema
func Open(dir string, a zim.Archive) (*Index, error) {
	m, err := ReadManifest(dir)
	if err != nil {
		return nil, fmt.Errorf("índice sin manifiesto válido (¿build a medias?): %w", err)
	}
	if err := m.Matches(a); err != nil {
		return nil, err
	}
	idx, err := bleve.Open(dir)
	if err != nil {
		return nil, err
	}
	return &Index{idx: idx, manifest: m}, nil
}

// Manifest devuelve el manifiesto con el que se abrió el índice (ya verificado).
func (i *Index) Manifest() Manifest { return i.manifest }

func (i *Index) Close() error { return i.idx.Close() }

// DocCount devuelve cuántos artículos hay indexados.
func (i *Index) DocCount() (uint64, error) { return i.idx.DocCount() }

// Search ejecuta la consulta full-text y devuelve los hits ordenados por rank
// (BM25) y el total de coincidencias. El match sobre título va potenciado ×3: un
// artículo cuyo TÍTULO casa pesa más que uno que solo menciona el término. Esto
// deja el orden fino al scoring del shim, igual que hoy con kiwix.
func (i *Index) Search(query string, limit int) ([]Hit, uint64, error) {
	if limit <= 0 {
		limit = 10
	}

	bodyQ := bleve.NewMatchQuery(query)
	bodyQ.SetField("body")
	titleQ := bleve.NewMatchQuery(query)
	titleQ.SetField("title")
	titleQ.SetBoost(3)

	req := bleve.NewSearchRequestOptions(bleve.NewDisjunctionQuery(bodyQ, titleQ), limit, 0, false)
	req.Fields = []string{"title"}
	req.Highlight = bleve.NewHighlight()
	req.Highlight.AddField("body")

	res, err := i.idx.Search(req)
	if err != nil {
		return nil, 0, err
	}

	hits := make([]Hit, 0, len(res.Hits))
	for _, h := range res.Hits {
		hit := Hit{Path: h.ID, Score: h.Score}
		if t, ok := h.Fields["title"].(string); ok {
			hit.Title = t
		}
		if frags, ok := h.Fragments["body"]; ok && len(frags) > 0 {
			hit.Snippet = strings.TrimSpace(frags[0])
		}
		hits = append(hits, hit)
	}
	return hits, res.Total, nil
}
