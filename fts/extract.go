// Paquete fts: full-text propio en Go puro (Fase C2). Índice bleve por colección,
// opt-in, construido iterando los artículos del .zim. Sustituye la dependencia de
// Xapian/kiwix para /search. La extracción de texto de aquí es el MISMO pipeline
// que luego alimenta la búsqueda-dentro y la IA (§7).
package fts

import (
	"io"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// maxArticleBytes acota el HTML que parseamos por artículo: un artículo gigante o
// corrupto no puede reventar la RAM del indexador (mismo criterio §17 del lector).
const maxArticleBytes = 4 << 20 // 4 MiB

// skipElements: elementos cuyo contenido NO es texto de artículo. Alineado con
// skipSnippetNode del shim para que el índice y los snippets coincidan.
var skipElements = map[atom.Atom]bool{
	atom.Script:   true,
	atom.Style:    true,
	atom.Nav:      true,
	atom.Footer:   true,
	atom.Header:   true,
	atom.Sup:      true, // referencias voladas [1], [2]…
	atom.Noscript: true,
	atom.Head:     true,
}

// skipByAttr descarta contenedores de navegación/metadatos por class o id, igual
// que el shim (navbox, metadata, mw-editsection, reference…).
func skipByAttr(v string) bool {
	v = strings.ToLower(v)
	return strings.Contains(v, "navbox") ||
		strings.Contains(v, "metadata") ||
		strings.Contains(v, "mw-editsection") ||
		strings.Contains(v, "reference") ||
		strings.Contains(v, "noprint") ||
		strings.Contains(v, "mw-jump") ||
		strings.Contains(v, "catlinks")
}

// isBlock: elementos de bloque; fuerzan separación para que "foo</p><p>bar" no se
// pegue como "foobar" al colapsar espacios.
func isBlock(a atom.Atom) bool {
	switch a {
	case atom.P, atom.Div, atom.Li, atom.Br, atom.Tr, atom.Td, atom.Th,
		atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6,
		atom.Section, atom.Article, atom.Ul, atom.Ol, atom.Table,
		atom.Blockquote, atom.Figcaption, atom.Dt, atom.Dd, atom.Caption:
		return true
	}
	return false
}

// extractText parsea el HTML del artículo y devuelve el cuerpo como texto plano
// ya colapsado (un solo espacio entre palabras). Un HTML roto degrada a lo que se
// pudo parsear, nunca hace panic (bien para un pool con ficheros no confiables).
//
// NOTA (FTS-AUDIT BUG-3): aquí NO se extrae el <title>. La versión anterior lo
// intentaba, pero el walk descartaba <head> (skipElements) antes de llegar al
// <title>, así que la rama era código muerto y el fallback e.Title() entraba
// siempre. Se eliminó a propósito: el título de una entrada lo dicta el DIRENT
// del ZIM (e.Title()), la misma fuente que usa el TitleIndex del suggest — una
// sola fuente de verdad, y el FTS y el suggest no pueden discrepar.
func extractText(r io.Reader) (body string) {
	doc, err := html.Parse(io.LimitReader(r, maxArticleBytes))
	if err != nil {
		return ""
	}

	var sb strings.Builder
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		switch n.Type {
		case html.ElementNode:
			if skipNode(n) {
				return
			}
			if isBlock(n.DataAtom) {
				sb.WriteByte(' ')
			}
		case html.TextNode:
			sb.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if n.Type == html.ElementNode && isBlock(n.DataAtom) {
			sb.WriteByte(' ')
		}
	}
	walk(doc)

	// Colapsa todo el blanco (espacios, saltos, tabs) a un único espacio.
	return strings.Join(strings.Fields(sb.String()), " ")
}

// skipNode: true si el nodo (y su subárbol) no debe aportar texto.
func skipNode(n *html.Node) bool {
	if skipElements[n.DataAtom] {
		return true
	}
	for _, a := range n.Attr {
		if (a.Key == "class" || a.Key == "id" || a.Key == "role") && skipByAttr(a.Val) {
			return true
		}
	}
	return false
}
