package zim

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

// Implementación real de Archive (paso 2: header → mimelist → dirent → pathindex).
// Lo que depende de clusters (Entry.Open, Metadata) llega en el paso 3; TitleIndex
// es Fase B. Todo lo demás — lookup por clave, redirects, portada, capacidades —
// funciona ya contra .zim reales.

type archive struct {
	f      io.ReaderAt // *os.File en producción; bytes.Reader en el fuzzing
	closer io.Closer   // nil para archives in-memory
	path   string      // ruta del .zim en disco; "" para archives in-memory (fuzzing)
	size   int64
	hdr    header
	mimes  []string
	caps   Capabilities
	limits Limits
	life   *lifecycle

	clusterCache *lruCache[clusterKey, *cachedCluster] // global por defecto (§4)
	direntCache  *lruCache[uint32, dirent]             // por archive, en entradas

	titleOnce sync.Once // el índice de títulos se construye UNA vez, al primer uso
	titleIdx  *titleIndex
	titleErr  error

	artOnce  sync.Once // conteo de artículos de portada (catálogo), cacheado
	artCount int

	closeOnce sync.Once
	closeErr  error
}

// openArchive: cuerpo de Open(). Abre el fichero, valida header + MIME list y
// detecta capacidades. Fail-fast: si algo no cuadra, se cierra el fichero y se
// devuelve un error legible.
func openArchive(ctx context.Context, path string, limits Limits) (Archive, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	a, err := initArchive(f, limits)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	a.path = path
	return a, nil
}

func initArchive(f *os.File, limits Limits) (*archive, error) {
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return newArchive(f, st.Size(), f, limits)
}

// newArchive: núcleo de la apertura sobre cualquier io.ReaderAt — producción entra
// por initArchive (*os.File); el fuzzing entra directo con un bytes.Reader.
func newArchive(f io.ReaderAt, size int64, closer io.Closer, limits Limits) (*archive, error) {
	var hb [headerSize]byte
	if _, err := f.ReadAt(hb[:], 0); err != nil {
		return nil, fmt.Errorf("%w: leyendo header: %v", ErrCorrupt, err)
	}
	hdr := parseHeader(hb[:])
	if err := hdr.validate(size); err != nil {
		return nil, err
	}
	mimes, err := readMimeList(f, int64(hdr.mimeListPos), int64(hdr.checksumPos), limits)
	if err != nil {
		return nil, err
	}
	a := &archive{
		f:            f,
		closer:       closer,
		size:         size,
		hdr:          hdr,
		mimes:        mimes,
		limits:       limits,
		life:         newLifecycle(),
		clusterCache: defaultClusterCache(),
		direntCache:  newLRUCache[uint32, dirent](direntCacheEntries),
	}
	a.caps = a.detectCapabilities()
	counters.openArchives.Add(1)
	return a, nil
}

// detectCapabilities (§13): por ESTRUCTURA, no por versión. Cada lookup es un
// O(log n) al abrir. Lo que exige descomprimir (extended clusters, validar el
// contenido de los listings) se detecta en su fase; aquí queda en false/presencia.
func (a *archive) detectCapabilities() Capabilities {
	exists := func(ns byte, path string) bool {
		_, _, err := a.findByKey(EntryKey{Namespace: ns, Path: path})
		return err == nil
	}
	return Capabilities{
		NewNamespaces:    a.hasNamespace('C'),
		HasMainPageEntry: exists('W', "mainPage") || a.hdr.mainPage != noMainPage,
		// Presencia estructural; la VALIDACIÓN del contenido (múltiplo de 4,
		// índices en rango, orden §21) es de la Fase B y puede degradarlas.
		HasTitleListingV1: exists('X', "listing/titleOrdered/v1"),
		HasTitleListingV0: exists('X', "listing/titleOrdered/v0"),
		HasLegacyTitleList: a.hdr.titlePtrPos >= headerSize &&
			uint64(a.hdr.entryCount) <= (a.hdr.checksumPos-a.hdr.titlePtrPos)/4,
		HasFullTextXapian: exists('X', "fulltext/xapian") || exists('Z', "/fulltextIndex/xapian"),
		HasTitleXapian:    exists('X', "title/xapian"),
		// HasExtendedCluster exige leer info bytes de clusters → paso 3.
	}
}

// hasNamespace: ¿existe al menos un dirent en el namespace ns? Búsqueda binaria
// del primer dirent con namespace >= ns y comprobación.
func (a *archive) hasNamespace(ns byte) bool {
	lo, hi := uint32(0), a.hdr.entryCount
	for lo < hi {
		mid := lo + (hi-lo)/2
		d, err := a.direntAtIndex(mid)
		if err != nil {
			return false
		}
		if d.namespace < ns {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo >= a.hdr.entryCount {
		return false
	}
	d, err := a.direntAtIndex(lo)
	return err == nil && d.namespace == ns
}

// withReader: envuelve toda operación pública que toca el fichero con el contador
// de lectores (§23), para que Close() no cierre el *os.File debajo de nadie.
func (a *archive) withReader(fn func() error) error {
	if err := a.life.acquire(); err != nil {
		return err
	}
	defer a.life.release()
	return fn()
}

func (a *archive) EntryAt(key EntryKey) (Entry, error) {
	var e Entry
	err := a.withReader(func() error {
		idx, d, err := a.findByKey(key)
		if err != nil {
			return err
		}
		e = &entry{a: a, idx: idx, d: d}
		return nil
	})
	return e, err
}

// EntryAtIndex: entrada por posición en la path pointer list — la base de
// cualquier iteración externa (diff contra kiwix, catálogos, auditorías).
func (a *archive) EntryAtIndex(idx uint32) (Entry, error) {
	var e Entry
	err := a.withReader(func() error {
		if idx >= a.hdr.entryCount {
			return fmt.Errorf("%w: índice %d con entryCount=%d", ErrNotFound, idx, a.hdr.entryCount)
		}
		d, err := a.direntAtIndex(idx)
		if err != nil {
			return err
		}
		e = &entry{a: a, idx: idx, d: d}
		return nil
	})
	return e, err
}

func (a *archive) EntryAtFullPath(full string) (Entry, error) {
	ns, path, ok := strings.Cut(full, "/")
	if !ok || len(ns) != 1 {
		return nil, fmt.Errorf("%w: full path %q malformado (esperado \"N/ruta\")", ErrNotFound, full)
	}
	return a.EntryAt(EntryKey{Namespace: ns[0], Path: path})
}

// MainPage (§2.1): primero W/mainPage (esquema moderno), luego header.mainPage.
// Los redirects se resuelven aquí: la portada que se devuelve es contenido real.
func (a *archive) MainPage() (Entry, error) {
	var e Entry
	err := a.withReader(func() error {
		idx, d, err := a.findByKey(EntryKey{Namespace: 'W', Path: "mainPage"})
		if err != nil {
			if a.hdr.mainPage == noMainPage {
				return fmt.Errorf("%w: el archivo no declara portada", ErrNotFound)
			}
			idx = a.hdr.mainPage
			if d, err = a.direntAtIndex(idx); err != nil {
				return err
			}
		}
		if idx, d, err = a.followRedirects(idx, d); err != nil {
			return err
		}
		e = &entry{a: a, idx: idx, d: d}
		return nil
	})
	return e, err
}

// Metadata: valor de M/<name> (M/Title, M/Language, M/Counter…). El valor es un
// blob normal, así que pasa por el mismo camino de clusters que el contenido.
func (a *archive) Metadata(name string) (string, error) {
	e, err := a.EntryAt(EntryKey{Namespace: 'M', Path: name})
	if err != nil {
		return "", err
	}
	rc, info, err := e.Open(context.Background())
	if err != nil {
		return "", err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, info.Size))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// TitleIndex (Fase B): construcción perezosa y cacheada — leer y normalizar los
// títulos de un ZIM de millones de entradas cuesta segundos y decenas de MB, así
// que solo se paga si el suggest se usa, y una sola vez. Con el caché de disco
// (`<zim>.tix`, titleindex_cache.go) ese coste se paga UNA VEZ EN LA VIDA del
// fichero: los arranques siguientes cargan el índice ya hecho.
func (a *archive) TitleIndex() (TitleIndex, error) {
	a.titleOnce.Do(func() {
		if ti := a.loadTitleIndexCache(); ti != nil {
			a.titleIdx = ti
			return
		}
		a.titleIdx, a.titleErr = a.buildTitleIndex()
		if a.titleErr == nil {
			a.saveTitleIndexCache(a.titleIdx) // best-effort: sin caché no pasa nada
		}
	})
	if a.titleErr != nil {
		return nil, a.titleErr
	}
	return a.titleIdx, nil
}

func (a *archive) Capabilities() Capabilities { return a.caps }
func (a *archive) EntryCount() uint32         { return a.hdr.entryCount }
func (a *archive) UUID() [16]byte             { return a.hdr.uuid }

// ArticleCount: artículos de portada, para el catálogo (§6). Es la longitud del
// listing titleOrdered/v1 (lo que kiwix llama articleCount) — una sola lectura de
// blob, cacheada; NO construye el índice de suggest (245 MB). Sin listing válido,
// cae a entryCount como cota superior barata.
func (a *archive) ArticleCount() int {
	a.artOnce.Do(func() {
		for _, key := range []EntryKey{{'X', "listing/titleOrdered/v1"}, {'X', "listing/titleOrdered/v0"}} {
			if idxs, err := a.listingIndices(key); err == nil {
				a.artCount = len(idxs)
				return
			}
		}
		a.artCount = int(a.hdr.entryCount)
	})
	return a.artCount
}

// Close: graceful (§23) — espera a los lectores activos y solo entonces cierra el
// fichero. Idempotente; tras esto, toda operación devuelve ErrClosed.
func (a *archive) Close() error {
	a.life.close()
	a.closeOnce.Do(func() {
		if a.closer != nil {
			a.closeErr = a.closer.Close()
		}
		// La caché global suelta las entradas de este UUID (§23).
		uuid := a.hdr.uuid
		a.clusterCache.removeIf(func(k clusterKey) bool { return k.uuid == uuid })
		counters.openArchives.Add(-1)
	})
	return a.closeErr
}

// ---- Entry ----

type entry struct {
	a   *archive
	idx uint32 // índice en la path pointer list
	d   dirent
}

func (e *entry) Key() EntryKey    { return e.d.key() }
func (e *entry) FullPath() string { return string(e.d.namespace) + "/" + e.d.path }
func (e *entry) Title() string    { return e.d.title }
func (e *entry) IsRedirect() bool { return e.d.kind == direntRedirect }

func (e *entry) RedirectTarget() (EntryKey, bool) {
	if e.d.kind != direntRedirect {
		return EntryKey{}, false
	}
	var key EntryKey
	err := e.a.withReader(func() error {
		d, err := e.a.direntAtIndex(e.d.redirect)
		if err != nil {
			return err
		}
		key = d.key()
		return nil
	})
	return key, err == nil
}

func (e *entry) MimeType() string {
	if e.d.kind != direntContent {
		return ""
	}
	return e.a.mimes[e.d.mimeIndex] // índice ya validado en readDirentAt
}

// Open entrega los bytes del blob que respalda la entrada. Si la entrada es un
// redirect, se resuelve hasta el contenido final (con tope §16) — el handler puede
// distinguir el caso ANTES vía IsRedirect() si prefiere emitir un 3xx.
//
// El reader devuelto retiene un lector del lifecycle (§23) hasta su Close(): un
// Close() del archive en paralelo espera, no arranca el fichero de debajo.
func (e *entry) Open(ctx context.Context) (io.ReadCloser, BlobInfo, error) {
	counters.blobOpens.Add(1)
	if err := e.a.life.acquire(); err != nil {
		counters.errors.Add(1)
		return nil, BlobInfo{}, err
	}
	rc, info, err := e.a.openEntryBlob(ctx, e.idx, e.d)
	if err != nil {
		e.a.life.release()
		counters.errors.Add(1)
		return nil, BlobInfo{}, err
	}
	// Si el reader de dentro sabe hacer Seek (blob sin comprimir o materializado
	// en RAM), se conserva la capacidad hacia fuera: el handler la detecta con un
	// type assert a io.ReadSeeker y sirve Range barato (§18).
	wrapped := &readerWithRelease{rc: rc, release: e.a.life.release}
	if _, ok := rc.(io.ReadSeeker); ok {
		return &readSeekerWithRelease{readerWithRelease: wrapped}, info, nil
	}
	return wrapped, info, nil
}

// openEntryBlob: resolución de redirects + apertura del blob. Llama con el
// lifecycle ya adquirido.
func (a *archive) openEntryBlob(ctx context.Context, idx uint32, d dirent) (io.ReadCloser, BlobInfo, error) {
	_, d, err := a.followRedirects(idx, d)
	if err != nil {
		return nil, BlobInfo{}, err
	}
	if d.kind != direntContent {
		return nil, BlobInfo{}, fmt.Errorf("%w: %c/%s no es contenido", ErrNotFound, d.namespace, d.path)
	}
	rc, size, seekable, err := a.openBlob(ctx, d.cluster, d.blob)
	if err != nil {
		return nil, BlobInfo{}, err
	}
	return rc, BlobInfo{
		Size:          size,
		MIME:          a.mimes[d.mimeIndex],
		Seekable:      seekable,
		Compressed:    !seekable,
		ClusterNumber: d.cluster,
		BlobNumber:    d.blob,
	}, nil
}

// readerWithRelease: suelta el contador de lectores (§23) exactamente una vez al
// cerrar, pase lo que pase con el reader de dentro.
type readerWithRelease struct {
	rc      io.ReadCloser
	release func()
	done    sync.Once
}

func (r *readerWithRelease) Read(p []byte) (int, error) {
	n, err := r.rc.Read(p)
	counters.bytesServed.Add(uint64(n))
	return n, err
}

func (r *readerWithRelease) Close() error {
	err := r.rc.Close()
	r.done.Do(r.release)
	return err
}

// readSeekerWithRelease: variante que además expone el Seek del reader interno.
type readSeekerWithRelease struct{ *readerWithRelease }

func (r *readSeekerWithRelease) Seek(offset int64, whence int) (int64, error) {
	return r.rc.(io.Seeker).Seek(offset, whence)
}
