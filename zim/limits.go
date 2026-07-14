package zim

import (
	"os"
	"strconv"
)

// Limits: topes defensivos OBLIGATORIOS (§16). Un ZIM corrupto o malicioso no
// controla memoria, CPU ni tamaños de lectura: un fichero malo en el pool no puede
// ser un DoS del daemon. Todos ajustables por env; los defaults se validan contra
// ZIMs reales antes de darlos por buenos.
//
// Unidades: los campos *Bytes van en bytes; los *MB en megabytes (igual que las
// env vars ZIM_MAX_*), para que el número del entorno case con el del código.
type Limits struct {
	MaxMimeListBytes         int64 // ZIM_MAX_MIME_LIST_BYTES
	MaxEntryStringBytes      int64 // ZIM_MAX_ENTRY_STRING_BYTES (path/title de un dirent)
	MaxParameterBytes        int64 // ZIM_MAX_PARAMETER_BYTES (parámetro extra del dirent)
	MaxRedirectDepth         int   // ZIM_MAX_REDIRECT_DEPTH (cadena de redirects → ErrRedirectCycle)
	MaxClusterCompressedMB   int64 // ZIM_MAX_CLUSTER_COMPRESSED_MB
	MaxDecompressedClusterMB int64 // ZIM_MAX_DECOMPRESSED_CLUSTER_MB
	MaxCachedClusterMB       int64 // ZIM_MAX_CACHED_CLUSTER_MB (presupuesto de la LRU)
	MaxBlobMB                int64 // ZIM_MAX_BLOB_MB (un blob individual)
	MaxDecompressionRatio    int64 // ZIM_MAX_DECOMPRESSION_RATIO (bomba de descompresión)

	// SuggestWordIndex: construir también el índice de PALABRAS del suggest, que
	// permite casar el prefijo a mitad de título (paridad con kiwix, §6) a costa de
	// ~duplicar la RAM del índice (medido: 245 MB con vs ~120 MB sin, en la
	// Wikipedia ES de 4.17M artículos). ZIM_SUGGEST_WORDS=0 lo desactiva en la Pi
	// con la RAM justa: el suggest queda como prefijo del título completo.
	SuggestWordIndex bool

	// TitleIndexCache: persistir el índice de títulos en disco (`<zim>.tix`, junto
	// al fichero) y cargarlo en los siguientes arranques en vez de reconstruirlo —
	// el rebuild de la Wikipedia ES cuesta ~20 s por arranque; la carga, una
	// fracción. ZIM_TITLE_CACHE=0 lo desactiva (p. ej. pool de solo lectura).
	TitleIndexCache bool
}

// DefaultLimits: defaults conservadores (pensados para la Pi). Se suben en x86 vía
// env si el baseline lo pide.
func DefaultLimits() Limits {
	return Limits{
		MaxMimeListBytes:         8 << 20,  // 8 MiB
		MaxEntryStringBytes:      1 << 20,  // 1 MiB
		MaxParameterBytes:        64 << 10, // 64 KiB
		MaxRedirectDepth:         8,
		MaxClusterCompressedMB:   1024,
		MaxDecompressedClusterMB: 512,
		MaxCachedClusterMB:       64,
		MaxBlobMB:                2048,
		MaxDecompressionRatio:    200,
		SuggestWordIndex:         true, // paridad con kiwix por defecto; se apaga por env
		TitleIndexCache:          true,
	}
}

// LimitsFromEnv: DefaultLimits() sobrescrito por las env vars ZIM_MAX_* presentes.
// Un valor inválido o ≤ 0 se ignora (se queda el default) — el entorno nunca puede
// aflojar un límite a algo peligroso por una errata.
func LimitsFromEnv() Limits {
	l := DefaultLimits()
	envInt64(&l.MaxMimeListBytes, "ZIM_MAX_MIME_LIST_BYTES")
	envInt64(&l.MaxEntryStringBytes, "ZIM_MAX_ENTRY_STRING_BYTES")
	envInt64(&l.MaxParameterBytes, "ZIM_MAX_PARAMETER_BYTES")
	envInt(&l.MaxRedirectDepth, "ZIM_MAX_REDIRECT_DEPTH")
	envInt64(&l.MaxClusterCompressedMB, "ZIM_MAX_CLUSTER_COMPRESSED_MB")
	envInt64(&l.MaxDecompressedClusterMB, "ZIM_MAX_DECOMPRESSED_CLUSTER_MB")
	envInt64(&l.MaxCachedClusterMB, "ZIM_MAX_CACHED_CLUSTER_MB")
	envInt64(&l.MaxBlobMB, "ZIM_MAX_BLOB_MB")
	envInt64(&l.MaxDecompressionRatio, "ZIM_MAX_DECOMPRESSION_RATIO")
	if os.Getenv("ZIM_SUGGEST_WORDS") == "0" {
		l.SuggestWordIndex = false
	}
	if os.Getenv("ZIM_TITLE_CACHE") == "0" {
		l.TitleIndexCache = false
	}
	return l
}

func envInt64(dst *int64, key string) {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			*dst = n
		}
	}
}

func envInt(dst *int, key string) {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			*dst = n
		}
	}
}
