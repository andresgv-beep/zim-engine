package zim

// Clusters (§3.4): donde viven los bytes de verdad. Offset del cluster i =
// clusterPtrList[i]; fin = offset del i+1 o checksumPos para el último. El primer
// byte es el info byte (compresión + flag extended); TODO lo demás va comprimido,
// incluida la tabla de offsets de blobs.
//
// No hay acceso aleatorio dentro de un stream zstd/xz → dos estrategias (§17):
//   C (completo):  total descomprimido ≤ ZIM_MAX_CACHED_CLUSTER_MB → se materializa
//                  entero y el blob se sirve desde RAM. (La LRU que lo retiene entre
//                  peticiones es el paso 4; aquí se materializa por petición.)
//   S (streaming): clusters grandes → descomprimir descartando hasta el blob y
//                  entregarlo en streaming, cortando el decoder al cerrar/cancelar.
//
// La tabla de offsets se recorre en O(1) de memoria: solo se retienen off[blob],
// off[blob+1] y off[último] — un blobCount hostil no puede reservar RAM (§16).

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// Valores del info byte (bits 0–3: compresión; bit 4: extended → offsets u64).
const (
	compNone  = 1
	compZlib  = 2 // deprecado
	compBzip2 = 3 // deprecado
	compXZ    = 4
	compZstd  = 5

	extendedFlag = 0x10
)

type clusterInfo struct {
	num         uint32
	start, end  int64 // [start, end) en el fichero; start apunta al info byte
	compression byte
	extended    bool
}

func (ci *clusterInfo) offSize() int64 {
	if ci.extended {
		return 8
	}
	return 4
}

// clusterPtrAt lee y valida el puntero i de la cluster pointer list.
func (a *archive) clusterPtrAt(i uint32) (int64, error) {
	var buf [8]byte
	if _, err := a.f.ReadAt(buf[:], int64(a.hdr.clusterPtrPos)+8*int64(i)); err != nil {
		return 0, fmt.Errorf("%w: leyendo cluster pointer %d: %v", ErrCorrupt, i, err)
	}
	ptr := binary.LittleEndian.Uint64(buf[:])
	if ptr < headerSize || ptr >= a.hdr.checksumPos {
		return 0, fmt.Errorf("%w: cluster pointer %d → %d fuera de rango", ErrCorrupt, i, ptr)
	}
	return int64(ptr), nil
}

// clusterAt localiza el cluster num, valida sus límites (monotónicos, §16) y lee
// su info byte.
func (a *archive) clusterAt(num uint32) (clusterInfo, error) {
	if num >= a.hdr.clusterCount {
		return clusterInfo{}, fmt.Errorf("%w: cluster %d con clusterCount=%d", ErrCorrupt, num, a.hdr.clusterCount)
	}
	start, err := a.clusterPtrAt(num)
	if err != nil {
		return clusterInfo{}, err
	}
	end := int64(a.hdr.checksumPos)
	if num+1 < a.hdr.clusterCount {
		if end, err = a.clusterPtrAt(num + 1); err != nil {
			return clusterInfo{}, err
		}
	}
	if end <= start { // offsets monotónicos: un cluster mide al menos su info byte
		return clusterInfo{}, fmt.Errorf("%w: cluster %d con límites [%d, %d)", ErrCorrupt, num, start, end)
	}
	if end-start > a.limits.MaxClusterCompressedMB<<20 {
		return clusterInfo{}, fmt.Errorf("%w: cluster %d de %d bytes comprimidos", ErrResourceLimit, num, end-start)
	}

	var info [1]byte
	if _, err := a.f.ReadAt(info[:], start); err != nil {
		return clusterInfo{}, fmt.Errorf("%w: leyendo info byte del cluster %d: %v", ErrCorrupt, num, err)
	}
	ci := clusterInfo{
		num:         num,
		start:       start,
		end:         end,
		compression: info[0] & 0x0F,
		extended:    info[0]&extendedFlag != 0,
	}
	switch ci.compression {
	case compNone, compXZ, compZstd:
		return ci, nil
	case compZlib, compBzip2:
		return clusterInfo{}, fmt.Errorf("%w: cluster %d usa compresión deprecada %d",
			ErrUnsupportedCompression, num, ci.compression)
	default:
		return clusterInfo{}, fmt.Errorf("%w: cluster %d con info byte de compresión %d",
			ErrUnsupportedCompression, num, ci.compression)
	}
}

// openBlob abre el blob (cluster, blob) y devuelve su reader, tamaño y si es
// seekable (cluster sin compresión → Range barato, §18).
func (a *archive) openBlob(ctx context.Context, cluster, blob uint32) (io.ReadCloser, int64, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, false, err
	}
	ci, err := a.clusterAt(cluster)
	if err != nil {
		return nil, 0, false, err
	}
	if ci.compression == compNone {
		rc, size, err := a.openRawBlob(ctx, ci, blob)
		return rc, size, true, err
	}
	rc, size, err := a.openCompressedBlob(ctx, ci, blob)
	return rc, size, false, err
}

// openRawBlob: cluster sin compresión — la tabla de offsets y el blob se leen
// directo del fichero con ReadAt, sin materializar nada.
func (a *archive) openRawBlob(ctx context.Context, ci clusterInfo, blob uint32) (io.ReadCloser, int64, error) {
	osz := ci.offSize()
	dataLen := ci.end - ci.start - 1 // área de offsets + blobs

	readOff := func(i int64) (int64, error) {
		var buf [8]byte
		if _, err := a.f.ReadAt(buf[:osz], ci.start+1+i*osz); err != nil {
			return 0, fmt.Errorf("%w: leyendo offset %d del cluster %d: %v", ErrCorrupt, i, ci.num, err)
		}
		if ci.extended {
			return int64(binary.LittleEndian.Uint64(buf[:])), nil
		}
		return int64(binary.LittleEndian.Uint32(buf[:4])), nil
	}

	off0, err := readOff(0)
	if err != nil {
		return nil, 0, err
	}
	blobCount, err := ci.blobCount(off0, dataLen)
	if err != nil {
		return nil, 0, err
	}
	if int64(blob) >= blobCount {
		return nil, 0, fmt.Errorf("%w: blob %d con %d blobs en el cluster %d", ErrCorrupt, blob, blobCount, ci.num)
	}
	bstart, err := readOff(int64(blob))
	if err != nil {
		return nil, 0, err
	}
	bend, err := readOff(int64(blob) + 1)
	if err != nil {
		return nil, 0, err
	}
	if err := ci.validateBlobBounds(bstart, bend, off0, dataLen, a.limits); err != nil {
		return nil, 0, err
	}
	size := bend - bstart
	// ReadSeekCloser de verdad: el blob vive sin comprimir en el fichero, así que
	// Seek es gratis (base del Range barato del handler, §18 nivel 1).
	return &seekableBlob{ctx: ctx, sec: io.NewSectionReader(a.f, ci.start+1+bstart, size)}, size, nil
}

// seekableBlob: reader con Seek para blobs en clusters sin compresión. El ctx se
// comprueba en cada Read (cancelación §17).
type seekableBlob struct {
	ctx context.Context
	sec *io.SectionReader
}

func (b *seekableBlob) Read(p []byte) (int, error) {
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	return b.sec.Read(p)
}

func (b *seekableBlob) Seek(offset int64, whence int) (int64, error) {
	return b.sec.Seek(offset, whence)
}

func (b *seekableBlob) Close() error { return nil }

// seekableBytes: ReadSeekCloser sobre un blob ya materializado en RAM (caché de
// clusters / estrategia C) — Seek gratis también.
type seekableBytes struct{ *bytes.Reader }

func (seekableBytes) Close() error { return nil }

func errBlobOutOfRange(blob uint32, count int64, cluster uint32) error {
	return fmt.Errorf("%w: blob %d con %d blobs en el cluster %d", ErrCorrupt, blob, count, cluster)
}

// openCompressedBlob: primero la caché (§4); en miss, descomprime la tabla y
// decide S o C (§17). La estrategia C materializa el cluster y lo deja en la LRU
// global — el siguiente blob del mismo artículo es RAM.
func (a *archive) openCompressedBlob(ctx context.Context, ci clusterInfo, blob uint32) (io.ReadCloser, int64, error) {
	key := clusterKey{uuid: a.hdr.uuid, num: ci.num}
	if cc, ok := a.clusterCache.get(key); ok {
		counters.clusterCacheHits.Add(1)
		b, err := cc.blobSlice(blob, ci.num)
		if err != nil {
			return nil, 0, err
		}
		return seekableBytes{bytes.NewReader(b)}, int64(len(b)), nil
	}
	counters.clusterCacheMisses.Add(1)

	compSize := ci.end - ci.start - 1
	src := &ctxReader{ctx: ctx, r: bufio.NewReaderSize(io.NewSectionReader(a.f, ci.start+1, compSize), 64<<10)}
	dec, err := a.newDecoder(ci.compression, src)
	if err != nil {
		return nil, 0, err
	}
	// Si el contexto murió, el error real es la cancelación — no un ErrCorrupt
	// fabricado por el decoder leyendo de un ctxReader ya cortado.
	closeOnErr := func(e error) (io.ReadCloser, int64, error) {
		dec.Close()
		if cerr := ctx.Err(); cerr != nil {
			e = cerr
		}
		return nil, 0, e
	}

	osz := ci.offSize()
	readOff := func() (int64, error) {
		var buf [8]byte
		if _, err := io.ReadFull(dec, buf[:osz]); err != nil {
			return 0, fmt.Errorf("%w: tabla de offsets del cluster %d: %v", ErrCorrupt, ci.num, err)
		}
		if ci.extended {
			return int64(binary.LittleEndian.Uint64(buf[:])), nil
		}
		return int64(binary.LittleEndian.Uint32(buf[:4])), nil
	}

	off0, err := readOff()
	if err != nil {
		return closeOnErr(err)
	}
	// Aquí no se conoce el tamaño descomprimido real → off0 se acota contra el
	// tope absoluto y el resto de la tabla se recorre sin retenerla.
	blobCount, err := ci.blobCount(off0, a.limits.MaxDecompressedClusterMB<<20)
	if err != nil {
		return closeOnErr(err)
	}
	if int64(blob) >= blobCount {
		return closeOnErr(fmt.Errorf("%w: blob %d con %d blobs en el cluster %d", ErrCorrupt, blob, blobCount, ci.num))
	}

	// La tabla se recorre reteniendo solo lo imprescindible. Si el cluster es
	// candidato a caché (blobCount acotado, §16), se retienen TODOS los offsets
	// para poder servir cualquier blob desde la entrada cacheada.
	retain := blobCount <= maxCacheableBlobs
	var offsets []int64
	if retain {
		offsets = make([]int64, 1, blobCount+1)
		offsets[0] = off0
	}
	var bstart, bend, prev, total int64
	prev = off0
	for i := int64(1); i <= blobCount; i++ {
		off, err := readOff()
		if err != nil {
			return closeOnErr(err)
		}
		if off < prev {
			return closeOnErr(fmt.Errorf("%w: tabla de offsets decreciente en el cluster %d", ErrCorrupt, ci.num))
		}
		prev = off
		if retain {
			offsets = append(offsets, off)
		}
		switch i {
		case int64(blob):
			bstart = off
		case int64(blob) + 1:
			bend = off
		}
		total = off
	}
	if blob == 0 {
		bstart = off0
	}

	if err := ci.validateBlobBounds(bstart, bend, off0, total, a.limits); err != nil {
		return closeOnErr(err)
	}
	if total > a.limits.MaxDecompressedClusterMB<<20 {
		return closeOnErr(fmt.Errorf("%w: cluster %d descomprime a %d bytes", ErrResourceLimit, ci.num, total))
	}
	if total > compSize*a.limits.MaxDecompressionRatio {
		return closeOnErr(fmt.Errorf("%w: cluster %d con ratio de descompresión %d:1",
			ErrResourceLimit, ci.num, total/max(compSize, 1)))
	}
	size := bend - bstart

	// Estrategia C: el cluster completo cabe en el presupuesto → materializar,
	// dejar en la LRU global y servir desde RAM (el 2º blob del mismo artículo
	// es gratis).
	if retain && total <= a.limits.MaxCachedClusterMB<<20 {
		data := make([]byte, total-off0)
		if _, err := io.ReadFull(dec, data); err != nil {
			return closeOnErr(fmt.Errorf("%w: datos del cluster %d: %v", ErrCorrupt, ci.num, err))
		}
		dec.Close()
		counters.fullClusterReads.Add(1)
		counters.decompressedBytes.Add(uint64(total))
		cc := &cachedCluster{offsets: offsets, data: data}
		a.clusterCache.put(key, cc, cc.cost())
		return seekableBytes{bytes.NewReader(data[bstart-off0 : bend-off0])}, size, nil
	}

	// Estrategia S: descartar hasta el blob y entregarlo en streaming. Close()
	// corta el decoder sin descomprimir el resto del cluster.
	if _, err := io.CopyN(io.Discard, dec, bstart-off0); err != nil {
		return closeOnErr(fmt.Errorf("%w: saltando al blob %d del cluster %d: %v", ErrCorrupt, blob, ci.num, err))
	}
	counters.streamingReads.Add(1)
	counters.decompressedBytes.Add(uint64(bend)) // consumido: tabla + descarte + blob
	return &blobStream{dec: dec, remaining: size, cluster: ci.num}, size, nil
}

// blobCount deduce y valida el nº de blobs a partir de off0 (§3.4): la tabla ocupa
// [0, off0) ⇒ off0 = (N+1)·offSize.
func (ci *clusterInfo) blobCount(off0, maxArea int64) (int64, error) {
	osz := ci.offSize()
	if off0 < osz || off0%osz != 0 || off0 > maxArea {
		return 0, fmt.Errorf("%w: off0=%d inválido en el cluster %d", ErrCorrupt, off0, ci.num)
	}
	return off0/osz - 1, nil
}

// validateBlobBounds: coherencia de los offsets del blob contra el área de datos y
// el límite de tamaño de blob (§16).
func (ci *clusterInfo) validateBlobBounds(bstart, bend, off0, areaEnd int64, limits Limits) error {
	if bstart < off0 || bend < bstart || bend > areaEnd {
		return fmt.Errorf("%w: blob [%d, %d) fuera del área [%d, %d) en el cluster %d",
			ErrCorrupt, bstart, bend, off0, areaEnd, ci.num)
	}
	if bend-bstart > limits.MaxBlobMB<<20 {
		return fmt.Errorf("%w: blob de %d bytes en el cluster %d", ErrResourceLimit, bend-bstart, ci.num)
	}
	return nil
}

// newDecoder: descompresor Go puro para el stream del cluster. zstd con memoria
// acotada (§16); xz (contenedor estándar de los ZIM con LZMA2).
func (a *archive) newDecoder(compression byte, r io.Reader) (io.ReadCloser, error) {
	switch compression {
	case compZstd:
		d, err := zstd.NewReader(r,
			zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderMaxMemory(uint64(a.limits.MaxDecompressedClusterMB)<<20))
		if err != nil {
			return nil, fmt.Errorf("%w: zstd: %v", ErrCorrupt, err)
		}
		return d.IOReadCloser(), nil
	case compXZ:
		xr, err := xz.NewReader(r)
		if err != nil {
			return nil, fmt.Errorf("%w: xz: %v", ErrCorrupt, err)
		}
		return io.NopCloser(xr), nil
	default:
		return nil, fmt.Errorf("%w: compresión %d", ErrUnsupportedCompression, compression)
	}
}

// ctxReader: inyecta la cancelación de contexto en la cadena de lectura — el
// decoder tira de aquí, así que un cliente que aborta corta la descompresión (§17).
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// blobStream: reader de la estrategia S — entrega exactamente el blob y al cerrar
// suelta el decoder sin procesar el resto del cluster.
type blobStream struct {
	dec       io.ReadCloser
	remaining int64
	cluster   uint32
}

func (b *blobStream) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.dec.Read(p)
	b.remaining -= int64(n)
	if err == io.EOF && b.remaining > 0 {
		// El stream murió antes de completar el blob: el cluster miente.
		err = fmt.Errorf("%w: blob truncado en el cluster %d", ErrCorrupt, b.cluster)
	}
	return n, err
}

func (b *blobStream) Close() error { return b.dec.Close() }
