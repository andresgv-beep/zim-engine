package zim

// Cachés (§4): aquí se gana o se pierde la Pi. Un artículo y sus 30 imágenes caen
// en pocos clusters — el segundo hit tiene que ser RAM, no re-descomprimir.
//
//   - Clusters descomprimidos: clave (uuid, clusterNum), presupuesto GLOBAL en
//     bytes (ZIM_MAX_CACHED_CLUSTER_MB), compartido entre todos los archives.
//   - Dirents: por archive, presupuesto en nº de entradas — la búsqueda binaria
//     toca los mismos pivotes una y otra vez.
//
// Seguridad de los buffers (§25 "eviction con buffer prestado"): las entradas son
// INMUTABLES y jamás se reciclan en un pool — la eviction solo suelta la referencia
// de la caché y el GC libera cuando el último reader termina. Sin refcount porque
// no hay reutilización de buffers; si algún día se añade pooling, el refcount del
// diseño entra aquí.
//
// LRU propio (regla §2: zim/ no importa nada del shim).

import (
	"container/list"
	"sync"
)

type lruCache[K comparable, V any] struct {
	mu     sync.Mutex
	budget int64
	used   int64
	ll     *list.List // frente = más reciente
	m      map[K]*list.Element
}

type lruItem[K comparable, V any] struct {
	k    K
	v    V
	cost int64
}

func newLRUCache[K comparable, V any](budget int64) *lruCache[K, V] {
	return &lruCache[K, V]{budget: budget, ll: list.New(), m: make(map[K]*list.Element)}
}

func (c *lruCache[K, V]) get(k K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.m[k]; ok {
		c.ll.MoveToFront(el)
		return el.Value.(*lruItem[K, V]).v, true
	}
	var zero V
	return zero, false
}

// put inserta (o reemplaza) y desaloja desde el fondo hasta caber. Una entrada más
// grande que el presupuesto entero no se guarda: se sirve sin cachear.
func (c *lruCache[K, V]) put(k K, v V, cost int64) {
	if cost > c.budget {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.m[k]; ok {
		c.used += cost - el.Value.(*lruItem[K, V]).cost
		el.Value = &lruItem[K, V]{k: k, v: v, cost: cost}
		c.ll.MoveToFront(el)
	} else {
		c.m[k] = c.ll.PushFront(&lruItem[K, V]{k: k, v: v, cost: cost})
		c.used += cost
	}
	for c.used > c.budget {
		back := c.ll.Back()
		if back == nil {
			break
		}
		it := back.Value.(*lruItem[K, V])
		c.ll.Remove(back)
		delete(c.m, it.k)
		c.used -= it.cost
	}
}

// removeIf desaloja todas las entradas cuya clave cumpla pred — p. ej. liberar las
// de un UUID al cerrar su archive (§23).
func (c *lruCache[K, V]) removeIf(pred func(K) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for el := c.ll.Front(); el != nil; {
		next := el.Next()
		if it := el.Value.(*lruItem[K, V]); pred(it.k) {
			c.ll.Remove(el)
			delete(c.m, it.k)
			c.used -= it.cost
		}
		el = next
	}
}

func (c *lruCache[K, V]) bytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.used
}

// ---- Caché de clusters descomprimidos ----

type clusterKey struct {
	uuid [16]byte
	num  uint32
}

// cachedCluster: un cluster materializado por la estrategia C. offsets[0] = off0;
// data es el área de blobs (sin la tabla). Inmutable tras crearse.
type cachedCluster struct {
	offsets []int64
	data    []byte
}

func (cc *cachedCluster) cost() int64 { return int64(len(cc.data)) + 8*int64(len(cc.offsets)) }

// blobSlice devuelve los bytes del blob i (comparte el buffer inmutable).
func (cc *cachedCluster) blobSlice(blob uint32, clusterNum uint32) ([]byte, error) {
	if int64(blob)+1 >= int64(len(cc.offsets)) {
		return nil, errBlobOutOfRange(blob, int64(len(cc.offsets))-1, clusterNum)
	}
	off0 := cc.offsets[0]
	return cc.data[cc.offsets[blob]-off0 : cc.offsets[blob+1]-off0], nil
}

// Presupuesto GLOBAL (§4): una sola caché de clusters para el proceso entero, con
// el tope de ZIM_MAX_CACHED_CLUSTER_MB leído una vez. Los tests inyectan la suya
// por archive.
var (
	globalClusterOnce  sync.Once
	globalClusterCache *lruCache[clusterKey, *cachedCluster]
)

func defaultClusterCache() *lruCache[clusterKey, *cachedCluster] {
	globalClusterOnce.Do(func() {
		globalClusterCache = newLRUCache[clusterKey, *cachedCluster](LimitsFromEnv().MaxCachedClusterMB << 20)
	})
	return globalClusterCache
}

// maxCacheableBlobs: tope defensivo del nº de offsets que se retienen para cachear
// un cluster (§16) — un blobCount hostil no infla la RAM; el cluster raro que lo
// supere se sirve en streaming y listo. Los reales andan por cientos de blobs.
const maxCacheableBlobs = 65536

// direntCacheEntries: presupuesto (en entradas) de la caché de dirents por archive.
const direntCacheEntries = 10240
