package fts

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/andresgv-beep/zim-engine/zim"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/analysis/analyzer/standard"
	"github.com/blevesearch/bleve/v2/analysis/lang/ar"
	"github.com/blevesearch/bleve/v2/analysis/lang/de"
	"github.com/blevesearch/bleve/v2/analysis/lang/en"
	"github.com/blevesearch/bleve/v2/analysis/lang/es"
	"github.com/blevesearch/bleve/v2/analysis/lang/fr"
	"github.com/blevesearch/bleve/v2/analysis/lang/it"
	"github.com/blevesearch/bleve/v2/analysis/lang/nl"
	"github.com/blevesearch/bleve/v2/analysis/lang/pt"
	"github.com/blevesearch/bleve/v2/analysis/lang/ru"
	"github.com/blevesearch/bleve/v2/mapping"
)

// ErrIncompleteBuild: los errores reales del build superaron el umbral. El índice
// mentiría por omisión, así que no se escribe manifiesto (FTS-AUDIT BUG-2).
var ErrIncompleteBuild = errors.New("fts: build incompleto")

// docFields: nombres de campo del documento indexado. El ID del documento bleve es
// el FullPath de la entrada ("C/Saturno"), así que no hace falta guardarlo aparte.
type indexDoc struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// Progress: avance del job de indexado, para pintar en la CLI o el Panel.
type Progress struct {
	Scanned uint32 // entradas recorridas de la path pointer list
	Indexed int    // artículos realmente indexados
	Total   uint32 // total de entradas del .zim
}

// BuildOptions configura el job. Language sale de M/Language del .zim; "" ⇒ standard.
//
// StoreBody: guardar el texto del artículo en el índice permite snippets resaltados
// directamente de bleve, pero infla el índice (~76–109% del .zim medido). Con false
// solo se indexa (searchable, no almacenado) y el snippet se regenera leyendo el
// artículo del .zim al mostrar — mismo camino que fillMissingPreviews del shim.
type BuildOptions struct {
	Language   string
	BatchSize  int // 0 ⇒ 512
	StoreBody  bool
	OnProgress func(Progress)
}

// Build crea (reconstruyendo desde cero) el índice bleve en dir a partir del
// archive. Devuelve cuántos artículos se indexaron. Cancelable por ctx (§17): un
// job largo se corta limpio si se retira la colección o se apaga el proceso.
func Build(ctx context.Context, a zim.Archive, dir string, opts BuildOptions) (int, error) {
	// ctx interno cancelable: un fallo del Builder debe poder cortar workers y
	// dispatcher aunque el llamante no cancele.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if opts.BatchSize <= 0 {
		opts.BatchSize = 512
	}
	// Publicación atómica (INDEXER-CRASH-SAFETY.md, Capa 1): se construye en un
	// directorio APARTE y solo al final se cambia por el bueno de golpe. Así el
	// índice vivo NUNCA se toca durante el build → un apagón a mitad no lo corrompe
	// ni lo pierde. Reconcile primero, por si un swap anterior quedó a medias:
	// restaura el índice bueno ANTES de tocar nada (si no, lo pisaríamos).
	if err := Reconcile(dir); err != nil {
		return 0, err
	}
	building := buildingDir(dir)
	if err := os.RemoveAll(building); err != nil { // restos de un build anterior
		return 0, err
	}

	// Builder offline de bleve: la API para índices write-once-read-many como este.
	// A diferencia del índice "vivo" (bleve.New + Batch), el Builder consolida los
	// segmentos al cerrar y NO deja huérfanos en disco — medido: el camino vivo
	// dejaba ~2× de disco fantasma (28.8 vs 14.4 MiB en es_climate) hasta una
	// purga asíncrona de scorch con la que no se puede contar.
	bld, err := bleve.NewBuilder(building, buildIndexMapping(opts.Language, opts.StoreBody),
		map[string]interface{}{"batchSize": opts.BatchSize})
	if err != nil {
		return 0, err
	}

	// Namespace de artículos según esquema: 'C' moderno, 'A' legacy (§13).
	artNS := byte('A')
	if a.Capabilities().NewNamespaces {
		artNS = 'C'
	}

	total := a.EntryCount()

	// Pipeline paralelo: lo caro es la extracción (abrir entrada + descomprimir +
	// parsear HTML), CPU-bound → pool de workers. El Builder recibe los docs desde
	// un ÚNICO consumidor. El dispatch va por RANGOS
	// contiguos de la path pointer list: entradas vecinas comparten cluster, así
	// cada worker explota la LRU en vez de pelearse por descomprimir lo mismo.
	//
	// Nota de determinismo: el ranking y el conteo son idénticos siempre (BM25 va
	// por estadísticas del corpus, no por orden de inserción), pero el layout de
	// segmentos del índice ya NO es byte-a-byte reproducible. Se acepta: el gate
	// de determinismo §5 aplica al índice de títulos, no al FTS.
	const chunk = 64
	workers := runtime.GOMAXPROCS(0)
	if workers > 8 {
		workers = 8 // el batcher y el disco saturan antes; más workers = RAM inútil
	}

	type doc struct {
		id string
		d  indexDoc
	}
	// Cada chunk viaja con su número de secuencia y el batcher REORDENA: los docs
	// entran a bleve en el orden de la path pointer list aunque la extracción sea
	// paralela — inserción determinista, independiente del número de workers. El
	// buffer de pendientes queda acotado por construcción: como mucho hay
	// (workers + cap(jobs)) chunks en vuelo fuera de orden.
	type job struct {
		seq    int
		lo, hi uint32
	}
	type chunkResult struct {
		seq  int
		docs []doc
	}
	jobs := make(chan job, workers)
	results := make(chan chunkResult, workers)

	var scanned atomic.Uint32
	// FTS-AUDIT BUG-2: los errores dejan de tragarse en silencio. Se cuentan por
	// tipo, y al final el build FALLA si los errores reales superan el umbral —
	// un índice a medias que se declara completo es peor que no tener índice.
	var candidates, skipped, failed atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				out := make([]doc, 0, 8)
				for i := j.lo; i < j.hi; i++ {
					scanned.Add(1)
					e, err := a.EntryAtIndex(i)
					if err != nil {
						failed.Add(1) // error de lectura real (dirent ilegible)
						continue
					}
					if e.IsRedirect() || e.Key().Namespace != artNS || !isHTML(e.MimeType()) {
						continue // no es candidato: ni cuenta ni es error
					}
					candidates.Add(1)
					rc, _, err := e.Open(ctx)
					if err != nil {
						if ctx.Err() != nil {
							return // cancelación: no es un fallo del ZIM
						}
						failed.Add(1) // cluster ilegible, límite §16…
						continue
					}
					body := extractText(rc)
					rc.Close()
					if body == "" {
						skipped.Add(1) // sin texto útil (vacías, solo-imagen): legítimo
						continue
					}
					out = append(out, doc{e.FullPath(), indexDoc{Title: e.Title(), Body: body}})
				}
				select {
				case results <- chunkResult{j.seq, out}:
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	go func() {
		defer close(jobs)
		seq := 0
		for lo := uint32(0); lo < total; lo += chunk {
			hi := lo + chunk
			if hi > total {
				hi = total
			}
			select {
			case jobs <- job{seq, lo, hi}:
				seq++
			case <-ctx.Done():
				return
			}
		}
	}()

	go func() { wg.Wait(); close(results) }()

	indexed, lastReport := 0, 0
	next := 0
	pending := map[int][]doc{}
	var indexErr error
consume:
	for r := range results {
		pending[r.seq] = r.docs
		for {
			ds, ok := pending[next]
			if !ok {
				break
			}
			delete(pending, next)
			next++
			for _, d := range ds {
				if err := bld.Index(d.id, d.d); err != nil {
					// FTS-AUDIT BUG-2: un fallo del Builder no es "un artículo
					// menos" — es el índice roto (disco lleno, segmento corrupto).
					// Antes: continue silencioso. Ahora: se aborta el build.
					indexErr = fmt.Errorf("bld.Index(%s): %w", d.id, err)
					cancel() // corta workers y dispatcher
					break consume
				}
				indexed++
			}
			if opts.OnProgress != nil && indexed-lastReport >= 500 {
				lastReport = indexed
				opts.OnProgress(Progress{Scanned: scanned.Load(), Indexed: indexed, Total: total})
			}
		}
	}
	// Drena results para que los workers no queden bloqueados enviando.
	for range results {
	}

	tally := Tally{
		Candidates: int(candidates.Load()),
		Indexed:    indexed,
		Skipped:    int(skipped.Load()),
		Failed:     int(failed.Load()),
	}

	// Si el builder falló o el ctx cortó el job, un índice a medias no vale nada:
	// cerrar el builder, borrar el directorio y reportar la causa real.
	if indexErr != nil {
		bld.Close()
		os.RemoveAll(building)
		return indexed, indexErr
	}
	if err := ctx.Err(); err != nil {
		bld.Close()
		os.RemoveAll(building)
		return indexed, err
	}

	// CERO artículos indexados = no hay índice que escribir. Colecciones de solo
	// vídeo/JS (p. ej. cursos interactivos) no tienen texto extraíble: la
	// colección queda en búsqueda por título y ya. Además, el Builder de bleve
	// (v2.6.0, scorch/builder.go:264) hace PANIC en Close() con 0 docs — se
	// cierra tras recover y se limpia el directorio. Descubierto con un ZIM real
	// (avanti-conics1: 215 entradas, 0 con texto).
	if indexed == 0 {
		func() {
			defer func() { _ = recover() }()
			bld.Close()
		}()
		os.RemoveAll(building)
		return 0, nil
	}

	// Umbral de completitud (FTS-AUDIT BUG-2): si los errores REALES superan el
	// 1% de los candidatos, el índice miente por omisión → no merece manifiesto.
	// (Skipped no cuenta: una página sin texto es legítima, no un fallo.)
	if tally.Candidates > 0 && tally.Failed*100 > tally.Candidates {
		bld.Close()
		os.RemoveAll(building)
		return indexed, fmt.Errorf("%w: %d de %d candidatos fallaron (>1%%): índice incompleto, no se escribe manifiesto",
			ErrIncompleteBuild, tally.Failed, tally.Candidates)
	}

	// Close del Builder = el merge final: consolida los segmentos y deja el índice
	// compacto. Aún en el directorio de construcción, no en el vivo.
	if err := bld.Close(); err != nil {
		return indexed, err
	}

	// El manifiesto va DESPUÉS del Close: su presencia certifica build completo Y
	// honesto (el tally viaja dentro). Se escribe en building; a partir de aquí,
	// "building con manifiesto" = índice listo para publicar (lo que Reconcile
	// reconoce y promueve si un corte pilla el swap por medio).
	if err := writeManifest(building, newManifest(a, opts, tally)); err != nil {
		return indexed, err
	}

	// Cambio atómico building → dir. AQUÍ, y solo aquí, se sustituye el índice
	// vivo, de una vez y con el nuevo ya entero y sellado (Capa 1).
	if err := promote(building, dir); err != nil {
		return indexed, err
	}

	if opts.OnProgress != nil {
		opts.OnProgress(Progress{Scanned: total, Indexed: indexed, Total: total})
	}
	return indexed, nil
}

func isHTML(mime string) bool {
	return strings.HasPrefix(mime, "text/html")
}

// buildIndexMapping arma el mapping bleve: title y body como texto con el analizador
// del idioma (stemming + stop words). El título se guarda siempre (barato, se pinta
// en resultados); el body solo con storeBody — y con él los term vectors, que solo
// sirven para el highlight de bleve sobre texto almacenado.
func buildIndexMapping(lang string, storeBody bool) mapping.IndexMapping {
	an := analyzerFor(lang)

	field := func(store, vectors bool) *mapping.FieldMapping {
		fm := bleve.NewTextFieldMapping()
		fm.Analyzer = an
		fm.Store = store
		fm.IncludeTermVectors = vectors
		return fm
	}

	dm := bleve.NewDocumentMapping()
	dm.AddFieldMappingsAt("title", field(true, storeBody))
	dm.AddFieldMappingsAt("body", field(storeBody, storeBody))

	im := bleve.NewIndexMapping()
	im.DefaultAnalyzer = an
	im.DefaultMapping = dm
	return im
}

// AnalyzerName devuelve el nombre del analizador bleve que se usaría para un código
// de idioma dado. Expuesto para diagnóstico (p. ej. la CLI lo muestra al indexar).
func AnalyzerName(lang string) string { return analyzerFor(lang) }

// analyzerFor mapea el código M/Language (ISO 639-1 de 2 letras, o 639-3 de 3) al
// analizador bleve. Idioma desconocido ⇒ standard (unicode + lowercase, sin stemmer)
// para no romper nada: siempre indexa, aunque sin stemming específico.
func analyzerFor(lang string) string {
	switch normLang(lang) {
	case "es":
		return es.AnalyzerName
	case "en":
		return en.AnalyzerName
	case "fr":
		return fr.AnalyzerName
	case "de":
		return de.AnalyzerName
	case "it":
		return it.AnalyzerName
	case "pt":
		return pt.AnalyzerName
	case "ru":
		return ru.AnalyzerName
	case "ar":
		return ar.AnalyzerName
	case "nl":
		return nl.AnalyzerName
	}
	return standard.Name
}

// normLang normaliza el código: primer subtag, minúsculas, y traduce los 639-3 más
// comunes a su 639-1. "spa" → "es", "es-419" → "es", "chu" → "chu" (→ standard).
func normLang(lang string) string {
	lang = strings.ToLower(strings.TrimSpace(lang))
	if i := strings.IndexAny(lang, "-_"); i > 0 {
		lang = lang[:i]
	}
	if len(lang) == 2 {
		return lang
	}
	switch lang {
	case "spa":
		return "es"
	case "eng":
		return "en"
	case "fra", "fre":
		return "fr"
	case "deu", "ger":
		return "de"
	case "ita":
		return "it"
	case "por":
		return "pt"
	case "rus":
		return "ru"
	case "ara":
		return "ar"
	case "nld", "dut":
		return "nl"
	}
	return lang // 3-letras sin mapear → caerá en standard
}
