package zim

// Observabilidad (§24): sin métricas no hay comparación con kiwix ni afinado de
// cachés. El paquete expone CONTADORES; el shim los vuelca donde ya vuelca lo demás
// (zim/ no importa su logger). Duraciones de petición, Range y errores por código
// HTTP viven en el handler, que es quien conoce la petición.

import "sync/atomic"

// Metrics: snapshot de los contadores del paquete (globales al proceso, igual que
// la caché de clusters).
type Metrics struct {
	OpenArchives       int64  // archives abiertos ahora mismo
	BlobOpens          uint64 // zim_requests_total (aperturas de blob)
	BytesServed        uint64 // zim_bytes_served_total
	ClusterCacheHits   uint64 // zim_cluster_cache_hits_total
	ClusterCacheMisses uint64 // zim_cluster_cache_misses_total
	ClusterCacheBytes  int64  // zim_cluster_cache_bytes (gauge)
	DecompressedBytes  uint64 // zim_decompressed_bytes_total
	StreamingReads     uint64 // zim_streaming_reads_total (estrategia S)
	FullClusterReads   uint64 // zim_full_cluster_reads_total (estrategia C)
	Errors             uint64 // zim_errors_total (fallos al abrir blobs/entradas)
}

var counters struct {
	openArchives       atomic.Int64
	blobOpens          atomic.Uint64
	bytesServed        atomic.Uint64
	clusterCacheHits   atomic.Uint64
	clusterCacheMisses atomic.Uint64
	decompressedBytes  atomic.Uint64
	streamingReads     atomic.Uint64
	fullClusterReads   atomic.Uint64
	errors             atomic.Uint64
}

// Stats devuelve el snapshot actual de los contadores del paquete.
func Stats() Metrics {
	m := Metrics{
		OpenArchives:       counters.openArchives.Load(),
		BlobOpens:          counters.blobOpens.Load(),
		BytesServed:        counters.bytesServed.Load(),
		ClusterCacheHits:   counters.clusterCacheHits.Load(),
		ClusterCacheMisses: counters.clusterCacheMisses.Load(),
		DecompressedBytes:  counters.decompressedBytes.Load(),
		StreamingReads:     counters.streamingReads.Load(),
		FullClusterReads:   counters.fullClusterReads.Load(),
		Errors:             counters.errors.Load(),
	}
	m.ClusterCacheBytes = defaultClusterCache().bytes()
	return m
}
