package zim

// Normalización Unicode de búsqueda (§21): la búsqueda byte a byte no basta en
// español. NO es el collation de libzim y no pasa nada: es NUESTRA normalización,
// documentada y estable (§12 la declara).
//
// Regla de oro: la MISMA función normaliza al construir el índice, al buscar y al
// generar variantes de typo. Si esto cambia, el índice y la consulta cambian
// JUNTOS por construcción.
//
// Criterio:
//   - UTF-8 inválido → los bytes se quedan tal cual (buscables en crudo).
//   - NFD para separar los diacríticos → se descartan las marcas (Mn) → NFC.
//     "Árbol" → "arbol"; la ñ se pliega a n (mismo criterio en índice y consulta,
//     así que "espanol" encuentra "español" y "español" también).
//   - Apóstrofes tipográficos (’ ʼ) → apóstrofe ASCII (').
//   - Lowercase Unicode.
//
// El título ORIGINAL nunca se toca: se conserva en el dirent para mostrar.

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

func normalizeKey(s string) string {
	if !utf8.ValidString(s) {
		return s
	}
	d := norm.NFD.String(s)
	var b strings.Builder
	b.Grow(len(d))
	for _, r := range d {
		if unicode.Is(unicode.Mn, r) {
			continue // marca combinante: aquí caen tildes, diéresis y la virgulilla
		}
		switch r {
		case '’', 'ʼ': // ’ ʼ
			r = '\''
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return norm.NFC.String(b.String())
}
