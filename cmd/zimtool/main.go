// zimtool: CLI de prueba del motor ZIM nativo, en aislamiento total (cero kiwix,
// cero Docker). Se usa para validar el motor contra .zim REALES según se construye.
//
// FASE A · paso 3: parser + clusters (zstd/xz, estrategias S/C) ya son reales.
//
//	zimtool info <archivo.zim>           header, capacidades, portada
//	zimtool ls <archivo.zim> [n]         entradas well-known
//	zimtool at <archivo.zim> <N/ruta>    detalle de una entrada ("C/index.html")
//	zimtool cat <archivo.zim> <N/ruta>   bytes del blob a stdout
//	zimtool meta <archivo.zim>           metadata M/* habitual
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/andresgv-beep/zim-engine/fts"
	"github.com/andresgv-beep/zim-engine/zim"
)

func main() {
	if len(os.Args) < 3 {
		usage()
	}
	cmd, path := os.Args[1], os.Args[2]

	// "fts" consulta un índice ya construido. Desde FTS-AUDIT BUG-1 exige TAMBIÉN
	// el .zim: fts.Open verifica el manifiesto contra el archive, y sin archive no
	// hay verificación. Uso: zimtool fts <fichero.zim> <dir-índice> <query> [n]
	if cmd == "fts" {
		if len(os.Args) < 5 {
			usage()
		}
		limit := 8
		if len(os.Args) > 5 {
			if v, e := strconv.Atoi(os.Args[5]); e == nil && v > 0 {
				limit = v
			}
		}
		if err := ftsSearch(path, os.Args[3], os.Args[4], limit); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	a, err := zim.Open(context.Background(), path, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	defer a.Close()

	switch cmd {
	case "info":
		err = info(a, path)
	case "ls":
		n := 20
		if len(os.Args) > 3 {
			if v, e := strconv.Atoi(os.Args[3]); e == nil && v > 0 {
				n = v
			}
		}
		err = ls(a, n)
	case "at":
		if len(os.Args) < 4 {
			usage()
		}
		err = at(a, os.Args[3])
	case "cat":
		if len(os.Args) < 4 {
			usage()
		}
		err = cat(a, os.Args[3])
	case "meta":
		err = meta(a)
	case "bench":
		if len(os.Args) < 4 {
			usage()
		}
		n := 5
		if len(os.Args) > 4 {
			if v, e := strconv.Atoi(os.Args[4]); e == nil && v > 0 {
				n = v
			}
		}
		err = bench(a, os.Args[3], n)
	case "suggest":
		if len(os.Args) < 4 {
			usage()
		}
		limit := 8
		if len(os.Args) > 4 {
			if v, e := strconv.Atoi(os.Args[4]); e == nil && v > 0 {
				limit = v
			}
		}
		err = suggest(a, os.Args[3], limit)
	case "index":
		if len(os.Args) < 4 {
			usage()
		}
		store := !(len(os.Args) > 4 && os.Args[4] == "nostore")
		err = ftsIndex(a, os.Args[3], store)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "uso: zimtool info|ls|at|cat|meta|suggest|bench <archivo.zim> [args]")
	fmt.Fprintln(os.Stderr, "     zimtool index <archivo.zim> <dir-índice>   construye el índice full-text (C2)")
	fmt.Fprintln(os.Stderr, "     zimtool fts   <archivo.zim> <dir-índice> <consulta> [n]  busca (verifica índice↔zim)")
	os.Exit(2)
}

// ftsIndex construye el índice full-text (Fase C2) del .zim en dir, tomando el
// idioma de M/Language, y muestra progreso, tiempo y tamaño en disco resultante.
func ftsIndex(a zim.Archive, dir string, storeBody bool) error {
	lang, _ := a.Metadata("Language")
	fmt.Printf("indexando %d entradas → %s  (idioma M/Language=%q → analizador %q, storeBody=%v)\n",
		a.EntryCount(), dir, lang, fts.AnalyzerName(lang), storeBody)

	t0 := time.Now()
	n, err := fts.Build(context.Background(), a, dir, fts.BuildOptions{
		Language:  lang,
		StoreBody: storeBody,
		OnProgress: func(p fts.Progress) {
			fmt.Printf("\r  %d/%d entradas · %d artículos indexados", p.Scanned, p.Total, p.Indexed)
		},
	})
	fmt.Println()
	if err != nil {
		return err
	}
	dt := time.Since(t0)

	size := dirSize(dir)
	fmt.Printf("listo: %d artículos en %.1fs · índice %.1f MiB (%.0f%% del .zim)\n",
		n, dt.Seconds(), float64(size)/(1<<20), float64(size)/float64(zimSize(a, dir))*100)
	return nil
}

// ftsSearch abre el .zim, verifica el índice contra él (manifiesto, BUG-1) y pinta
// los hits (título, ruta, score y snippet, con el <mark> convertido a [ ]).
func ftsSearch(zimPath, dir, query string, limit int) error {
	a, err := zim.Open(context.Background(), zimPath, nil)
	if err != nil {
		return fmt.Errorf("abrir zim: %w", err)
	}
	defer a.Close()

	idx, err := fts.Open(dir, a) // verifica manifiesto: índice ajeno/viejo → error
	if err != nil {
		return err
	}
	m := idx.Manifest()
	fmt.Printf("manifiesto OK: zim=%.8s… candidatos=%d indexados=%d omitidos=%d fallidos=%d analizador=%s body=%v (%s)\n",
		m.ZimUUID, m.Candidates, m.Indexed, m.Skipped, m.Failed, m.Analyzer, m.StoreBody, m.BuiltAt)
	defer idx.Close()

	t0 := time.Now()
	hits, total, err := idx.Search(query, limit)
	if err != nil {
		return err
	}
	dt := time.Since(t0)

	dc, _ := idx.DocCount()
	fmt.Printf("«%s» → %d coincidencias en %d artículos · %.2f ms\n\n", query, total, dc, float64(dt.Microseconds())/1000)
	for i, h := range hits {
		fmt.Printf("%2d. [%.3f] %s\n    %s\n", i+1, h.Score, h.Title, h.Path)
		if h.Snippet != "" {
			snip := strings.NewReplacer("<mark>", "[", "</mark>", "]").Replace(h.Snippet)
			fmt.Printf("    %s\n", snip)
		}
		fmt.Println()
	}
	return nil
}

func dirSize(dir string) int64 {
	var total int64
	filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, e := d.Info(); e == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}

// zimSize: tamaño del .zim de origen, para el ratio índice/fuente. Se deduce del
// argumento original (os.Args[2]); si no se puede, evita división por cero.
func zimSize(_ zim.Archive, _ string) int64 {
	if len(os.Args) > 2 {
		if info, err := os.Stat(os.Args[2]); err == nil && info.Size() > 0 {
			return info.Size()
		}
	}
	return 1
}

func cat(a zim.Archive, full string) error {
	e, err := a.EntryAtFullPath(full)
	if err != nil {
		return err
	}
	rc, info, err := e.Open(context.Background())
	if err != nil {
		return err
	}
	defer rc.Close()
	fmt.Fprintf(os.Stderr, "%s: %d bytes, mime=%s, seekable=%v, cluster=%d, blob=%d\n",
		full, info.Size, info.MIME, info.Seekable, info.ClusterNumber, info.BlobNumber)
	_, err = io.Copy(os.Stdout, rc)
	return err
}

// bench: lee la misma entrada n veces y enseña el efecto de la LRU de clusters —
// la 1ª lectura descomprime, las siguientes salen de RAM.
func bench(a zim.Archive, full string, n int) error {
	for i := 0; i < n; i++ {
		t0 := time.Now()
		e, err := a.EntryAtFullPath(full)
		if err != nil {
			return err
		}
		rc, info, err := e.Open(context.Background())
		if err != nil {
			return err
		}
		nb, err := io.Copy(io.Discard, rc)
		rc.Close()
		if err != nil {
			return err
		}
		fmt.Printf("lectura %d: %8.2f ms  (%d bytes)\n", i+1, float64(time.Since(t0).Microseconds())/1000, nb)
		_ = info
	}
	s := zim.Stats()
	fmt.Printf("\nmétricas §24: opens=%d hits=%d misses=%d cacheBytes=%d descomprimidos=%d servidos=%d\n",
		s.BlobOpens, s.ClusterCacheHits, s.ClusterCacheMisses, s.ClusterCacheBytes,
		s.DecompressedBytes, s.BytesServed)
	return nil
}

// suggest: búsqueda por título (Fase B). La primera llamada construye el índice
// (perezoso) — se cronometra aparte para ver lo que costaría en la Pi.
func suggest(a zim.Archive, term string, limit int) error {
	t0 := time.Now()
	ti, err := a.TitleIndex()
	if err != nil {
		return err
	}
	build := time.Since(t0)

	t0 = time.Now()
	keys, err := ti.Search(term, limit)
	if err != nil {
		return err
	}
	search := time.Since(t0)

	for _, k := range keys {
		e, err := a.EntryAt(k)
		if err != nil {
			return err
		}
		marker := ""
		if e.IsRedirect() {
			if tgt, ok := e.RedirectTarget(); ok {
				marker = fmt.Sprintf("  → %c/%s", tgt.Namespace, tgt.Path)
			}
		}
		fmt.Printf("%-50q %s%s\n", e.Title(), e.FullPath(), marker)
	}
	fmt.Printf("\níndice: %.0f ms (una vez) · búsqueda: %.2f ms · %d resultados\n",
		float64(build.Microseconds())/1000, float64(search.Microseconds())/1000, len(keys))
	return nil
}

func meta(a zim.Archive) error {
	for _, name := range []string{
		"Title", "Language", "Description", "Creator", "Publisher", "Date",
		"Counter", "Scraper", "Flavour", "Tags",
	} {
		v, err := a.Metadata(name)
		if err != nil {
			continue
		}
		if len(v) > 100 {
			v = v[:100] + "…"
		}
		fmt.Printf("M/%-12s = %q\n", name, v)
	}
	return nil
}

func info(a zim.Archive, path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	fmt.Printf("archivo    : %s\n", path)
	fmt.Printf("tamaño     : %d bytes (%.1f MiB)\n", st.Size(), float64(st.Size())/(1<<20))
	fmt.Printf("uuid       : %x\n", a.UUID())
	fmt.Printf("entradas   : %d\n", a.EntryCount())

	c := a.Capabilities()
	fmt.Printf("capacidades: newNS=%v mainPage=%v titleV1=%v titleV0=%v legacyTitle=%v xapianFT=%v xapianTitle=%v\n",
		c.NewNamespaces, c.HasMainPageEntry, c.HasTitleListingV1, c.HasTitleListingV0,
		c.HasLegacyTitleList, c.HasFullTextXapian, c.HasTitleXapian)

	if mp, err := a.MainPage(); err == nil {
		fmt.Printf("portada    : %s  (título: %q, mime: %s)\n", mp.FullPath(), mp.Title(), mp.MimeType())
	} else {
		fmt.Printf("portada    : %v\n", err)
	}
	return nil
}

func ls(a zim.Archive, n int) error {
	// Sin iterador público todavía (llega cuando lo pida la Fase B); listar =
	// portada + lookups conocidos. De momento: recorrer con EntryAtFullPath no
	// aplica, así que se listan las entradas well-known útiles para inspección.
	fmt.Printf("(ls muestra entradas well-known; el iterador completo llega con la Fase B)\n\n")
	for _, p := range []string{
		"W/mainPage",
		"M/Title", "M/Language", "M/Description", "M/Creator", "M/Date", "M/Counter",
		"M/Illustration_48x48@1",
		"X/listing/titleOrdered/v1", "X/listing/titleOrdered/v0",
		"X/fulltext/xapian", "X/title/xapian",
	} {
		e, err := a.EntryAtFullPath(p)
		if err != nil {
			continue
		}
		printEntry(e)
		if n--; n == 0 {
			break
		}
	}
	return nil
}

func at(a zim.Archive, full string) error {
	e, err := a.EntryAtFullPath(full)
	if err != nil {
		return err
	}
	printEntry(e)
	return nil
}

func printEntry(e zim.Entry) {
	if e.IsRedirect() {
		tgt, ok := e.RedirectTarget()
		fmt.Printf("%-40s → redirect a %c/%s (ok=%v)\n", e.FullPath(), tgt.Namespace, tgt.Path, ok)
		return
	}
	fmt.Printf("%-40s mime=%-30s título=%q\n", e.FullPath(), e.MimeType(), e.Title())
}
