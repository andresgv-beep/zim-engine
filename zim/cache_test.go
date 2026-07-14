package zim

// Tests del paso 4: LRU de clusters (hit/miss, eviction, buffer prestado, limpieza
// por UUID al cerrar), caché de dirents y métricas §24.

import (
	"context"
	"io"
	"sync"
	"testing"
)

// zimWithBlobs: un ZIM zstd con tres blobs en el cluster 0.
func zimWithBlobs(t *testing.T) []byte {
	t.Helper()
	return buildZIMC(t, []string{"text/plain"}, []tEntry{
		{ns: 'C', path: "a", mime: 0, content: []byte("contenido-a")},
		{ns: 'C', path: "b", mime: 0, content: []byte("contenido-b")},
		{ns: 'C', path: "c", mime: 0, content: []byte("contenido-c")},
	}, noMainPage, compZstd, false)
}

func TestClusterCacheHit(t *testing.T) {
	a, err := openZIMBytes(t, zimWithBlobs(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	// Caché propia para poder afirmar sobre su estado sin ruido de otros tests.
	cache := newLRUCache[clusterKey, *cachedCluster](64 << 20)
	a.(*archive).clusterCache = cache

	before := Stats()
	if got, _ := readEntry(t, a, "C/a"); string(got) != "contenido-a" {
		t.Fatalf("primer blob = %q", got)
	}
	// Segundo blob del MISMO cluster: tiene que salir de la caché (§4 — el caso
	// "artículo + sus 30 imágenes").
	if got, _ := readEntry(t, a, "C/b"); string(got) != "contenido-b" {
		t.Fatalf("segundo blob = %q", got)
	}
	after := Stats()
	if hits := after.ClusterCacheHits - before.ClusterCacheHits; hits != 1 {
		t.Errorf("hits = %d, esperado 1", hits)
	}
	if misses := after.ClusterCacheMisses - before.ClusterCacheMisses; misses != 1 {
		t.Errorf("misses = %d, esperado 1", misses)
	}
	if cache.bytes() == 0 {
		t.Error("la caché quedó vacía tras una lectura con estrategia C")
	}
}

func TestClusterCacheEvictionAndBorrowedBuffer(t *testing.T) {
	dataA := zimWithBlobs(t)
	a, err := openZIMBytes(t, dataA, nil)
	if err != nil {
		t.Fatal(err)
	}
	arch := a.(*archive)
	// Presupuesto mínimo: cabe UN cluster de test y poco más.
	cache := newLRUCache[clusterKey, *cachedCluster](128)
	arch.clusterCache = cache

	// Poblar la caché y quedarse un reader abierto sobre la entrada cacheada.
	e, err := a.EntryAtFullPath("C/a")
	if err != nil {
		t.Fatal(err)
	}
	rc, _, err := e.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	// Forzar la eviction desalojando a mano (equivale a que otro cluster grande
	// entre y la LRU tire este). El buffer prestado NO puede corromperse (§25).
	cache.removeIf(func(clusterKey) bool { return true })
	if cache.bytes() != 0 {
		t.Fatalf("caché no vaciada: %d bytes", cache.bytes())
	}
	got, err := io.ReadAll(rc)
	if err != nil || string(got) != "contenido-a" {
		t.Errorf("lectura con buffer desalojado: %q, %v", got, err)
	}
}

func TestClusterCacheFreedOnClose(t *testing.T) {
	a, err := openZIMBytes(t, zimWithBlobs(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	arch := a.(*archive)
	cache := newLRUCache[clusterKey, *cachedCluster](64 << 20)
	arch.clusterCache = cache

	readEntry(t, a, "C/a")
	if cache.bytes() == 0 {
		t.Fatal("la caché debería tener el cluster")
	}
	a.Close()
	if cache.bytes() != 0 {
		t.Errorf("Close no liberó las entradas del UUID: %d bytes", cache.bytes())
	}
}

func TestDirentCache(t *testing.T) {
	a, err := openZIMBytes(t, zimWithBlobs(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	arch := a.(*archive)
	if arch.direntCache.bytes() == 0 {
		t.Fatal("detectCapabilities ya debería haber poblado la caché de dirents")
	}
	// Dos lookups de la misma clave devuelven lo mismo (el 2º sale de la caché).
	e1, err := a.EntryAtFullPath("C/b")
	if err != nil {
		t.Fatal(err)
	}
	e2, err := a.EntryAtFullPath("C/b")
	if err != nil {
		t.Fatal(err)
	}
	if e1.Key() != e2.Key() || e1.Title() != e2.Title() {
		t.Errorf("lookups incoherentes: %+v vs %+v", e1.Key(), e2.Key())
	}
}

func TestLRUBasics(t *testing.T) {
	c := newLRUCache[int, string](10)
	c.put(1, "a", 4)
	c.put(2, "b", 4)
	if _, ok := c.get(1); !ok {
		t.Fatal("1 debería estar")
	}
	// 1 acaba de usarse → al meter 3 (no cabe todo) debe caer 2, el LRU real.
	c.put(3, "c", 4)
	if _, ok := c.get(2); ok {
		t.Error("2 debería haber sido desalojado")
	}
	if _, ok := c.get(1); !ok {
		t.Error("1 debería seguir (recién usado)")
	}
	// Una entrada más grande que el presupuesto no se guarda ni desaloja nada.
	c.put(4, "enorme", 100)
	if _, ok := c.get(4); ok {
		t.Error("4 no debería haberse guardado")
	}
	if _, ok := c.get(1); !ok {
		t.Error("1 no debería haber sido desalojado por una entrada imposible")
	}
	// Reemplazo: mismo coste contable, valor nuevo.
	c.put(1, "a2", 4)
	if v, _ := c.get(1); v != "a2" {
		t.Errorf("reemplazo: %q", v)
	}
}

func TestConcurrentReads(t *testing.T) {
	// 50 lectores del mismo blob + 50 de blobs distintos, con caché compartida
	// (matriz §25; el -race de verdad corre en Linux).
	a, err := openZIMBytes(t, zimWithBlobs(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	a.(*archive).clusterCache = newLRUCache[clusterKey, *cachedCluster](64 << 20)

	var wg sync.WaitGroup
	errs := make(chan error, 100)
	paths := []string{"C/a", "C/b", "C/c"}
	want := map[string]string{"C/a": "contenido-a", "C/b": "contenido-b", "C/c": "contenido-c"}
	for i := 0; i < 100; i++ {
		p := paths[i%len(paths)]
		wg.Add(1)
		go func() {
			defer wg.Done()
			e, err := a.EntryAtFullPath(p)
			if err != nil {
				errs <- err
				return
			}
			rc, _, err := e.Open(context.Background())
			if err != nil {
				errs <- err
				return
			}
			defer rc.Close()
			got, err := io.ReadAll(rc)
			if err != nil {
				errs <- err
				return
			}
			if string(got) != want[p] {
				errs <- io.ErrUnexpectedEOF
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("lector concurrente: %v", err)
	}
}
