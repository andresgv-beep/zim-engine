package zim

import (
	"context"
	"io"
)

// EntryKey: clave SIN ambigüedad — el namespace va separado del path. Imprescindible
// para que convivan el esquema moderno ('C' todo el contenido) y el legacy ('A'
// artículos, 'I' imágenes) sin colisiones.
type EntryKey struct {
	Namespace byte
	Path      string
}

// Capabilities: qué sabe hacer ESTE archivo, detectado por estructura (§13), NO por
// el número de versión del header. Una capacidad solo se activa tras VALIDAR su
// estructura; un ZIM raro degrada con fallback, jamás panic.
type Capabilities struct {
	NewNamespaces      bool
	HasMainPageEntry   bool
	HasTitleListingV1  bool // validada, no solo presente
	HasTitleListingV0  bool
	HasLegacyTitleList bool
	HasFullTextXapian  bool // informativo: NO lo usamos (full-text propio = C2)
	HasTitleXapian     bool
	HasExtendedCluster bool
}

// BlobInfo: metadatos del blob que respalda una entrada. Seekable marca los blobs
// en clusters SIN comprimir → Range barato con ReadAt directo (§18).
type BlobInfo struct {
	Size          int64
	MIME          string
	Seekable      bool
	Compressed    bool
	ClusterNumber uint32
	BlobNumber    uint32
}

// Entry: una entrada del ZIM (artículo, imagen, CSS, redirect…).
type Entry interface {
	Key() EntryKey
	FullPath() string // "C/Saturno" — helper para construir URLs
	Title() string
	IsRedirect() bool
	RedirectTarget() (EntryKey, bool)
	MimeType() string
	// Open entrega los bytes del blob. El ctx corta el decoder si el cliente
	// cancela (§17): no se descomprime de más tras un abort.
	Open(ctx context.Context) (io.ReadCloser, BlobInfo, error)
}

// TitleIndex: búsqueda por título/prefijo (Fase B). Search es determinista.
type TitleIndex interface {
	Search(prefix string, limit int) ([]EntryKey, error)
}

// Archive: un .zim abierto. Concurrencia segura por ReadAt (pread); un *os.File por
// archivo, sin seek compartido, sin mutex en el hot path de lectura.
type Archive interface {
	Metadata(name string) (string, error) // M/Title, M/Language, M/Counter…
	MainPage() (Entry, error)             // vía W/mainPage o header.mainPage
	EntryAt(key EntryKey) (Entry, error)
	EntryAtFullPath(full string) (Entry, error) // "C/Saturno" → EntryKey
	EntryAtIndex(idx uint32) (Entry, error)     // por posición en la path pointer list (iteración/diff)
	TitleIndex() (TitleIndex, error)            // Fase B
	Capabilities() Capabilities                 // §13
	EntryCount() uint32
	ArticleCount() int // artículos de portada (longitud del listing v1); catálogo
	UUID() [16]byte
	Close() error // graceful: no cierra con lectores activos (§23)
}

// Options: configuración de apertura. Limits vacío ⇒ LimitsFromEnv().
type Options struct {
	Limits *Limits
}

func (o *Options) limits() Limits {
	if o != nil && o.Limits != nil {
		return *o.Limits
	}
	return LimitsFromEnv()
}

// Open abre un .zim para lectura.
//
// FASE A · paso 2: header, MIME list, dirents y path index funcionan contra .zim
// reales (lookup por clave, redirects, portada, capacidades). Lo que exige
// descomprimir clusters — Entry.Open y Metadata — devuelve ErrNotImplemented hasta
// el paso 3 (cluster.go); TitleIndex es Fase B.
func Open(ctx context.Context, path string, opts *Options) (Archive, error) {
	return openArchive(ctx, path, opts.limits())
}
