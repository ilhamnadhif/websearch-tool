package fetchpage

import (
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// maxRenderedChars bounds how much text one page may yield. It is far more
// than any caller shows (see Options.MaxChars); the excess exists so that
// Options.Focus can pick the relevant part of a long page.
const maxRenderedChars = 100_000

type blockKind int

const (
	blockText blockKind = iota
	blockHeading
	blockItem
	blockRow
)

// block is one line of a page's text: a paragraph, a heading, a list item,
// or a table row. Keeping the kind lets Content show the page's structure
// (a "## " heading, a "- " item, cells joined by " | ") and lets focus
// selection keep a table's header row with the rows it picks.
type block struct {
	kind  blockKind
	text  string
	table int  // rows: which table, numbered from 1
	head  bool // rows: the table's first row, usually its column names
}

func (b block) String() string {
	switch b.kind {
	case blockHeading:
		return "## " + b.text
	case blockItem:
		return "- " + b.text
	default:
		return b.text
	}
}

func blocksText(blocks []block) string {
	lines := make([]string, len(blocks))
	for i, b := range blocks {
		lines[i] = b.String()
	}
	return strings.Join(lines, "\n")
}

func blocksLen(blocks []block) int {
	n := 0
	for _, b := range blocks {
		n += utf8.RuneCountInString(b.String()) + 1
	}
	return n
}

// renderer walks a DOM tree and emits blocks.
type renderer struct {
	blocks []block
	line   strings.Builder
	kind   blockKind
	tables int
	size   int
}

func renderBlocks(n *html.Node) []block {
	r := &renderer{}
	r.walk(n)
	r.flush()
	return dedupeAdjacent(r.blocks)
}

func (r *renderer) full() bool {
	return r.size >= maxRenderedChars
}

func (r *renderer) walk(n *html.Node) {
	if r.full() {
		return
	}

	switch n.Type {
	case html.TextNode:
		r.line.WriteString(n.Data)
		return
	case html.ElementNode:
	default:
		r.children(n)
		return
	}

	if skipElement(n) {
		return
	}

	switch n.DataAtom {
	case atom.Br:
		r.flush()
		return
	case atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6:
		r.flush()
		r.kind = blockHeading
		r.children(n)
		r.flush()
		return
	case atom.Li:
		r.flush()
		r.kind = blockItem
		r.children(n)
		r.flush()
		return
	case atom.Table:
		r.flush()
		if layoutTable(n) {
			r.children(n)
			r.flush()
		} else {
			r.table(n)
		}
		return
	}

	if blockElements[n.DataAtom] {
		r.flush()
		r.children(n)
		r.flush()
		return
	}

	r.children(n)
}

func (r *renderer) children(n *html.Node) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		r.walk(c)
	}
}

// flush ends the current line, emitting it as a block of the pending kind.
func (r *renderer) flush() {
	text := strings.Join(strings.Fields(r.line.String()), " ")
	r.line.Reset()

	kind := r.kind
	r.kind = blockText

	if text == "" {
		return
	}

	// A very long paragraph or list item is split at sentence ends, so that
	// focus selection can keep the part of it that matters; an item's
	// continuation reads as plain text under it. A heading that long is not
	// a heading, and is cut.
	if len(text) > 2*passageChars {
		switch kind {
		case blockText, blockItem:
			for i, part := range splitLongText(text) {
				if i == 0 {
					part.kind = kind
				}
				r.emit(part)
			}
			return
		case blockHeading:
			text = truncateRunes(text, passageChars/2)
		}
	}

	r.emit(block{kind: kind, text: text})
}

func (r *renderer) emit(b block) {
	r.blocks = append(r.blocks, b)
	r.size += utf8.RuneCountInString(b.text) + 1
}

// table emits one row block per row, cells joined by " | ". Rows of nested
// tables are flattened into their cell.
func (r *renderer) table(n *html.Node) {
	r.tables++
	id := r.tables
	first := true

	if caption := findChild(n, atom.Caption); caption != nil {
		if text := inlineText(caption); text != "" {
			r.emit(block{kind: blockText, text: text})
		}
	}

	for _, tr := range tableRows(n) {
		var cells []string
		for c := tr.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.ElementNode && (c.DataAtom == atom.Td || c.DataAtom == atom.Th) {
				if skipElement(c) {
					continue
				}
				cells = append(cells, inlineText(c))
			}
		}

		for len(cells) > 0 && cells[len(cells)-1] == "" {
			cells = cells[:len(cells)-1]
		}
		if len(cells) == 0 || strings.Join(cells, "") == "" {
			continue
		}

		// A row too long to be data -- whole passages in its cells -- is cut,
		// so it cannot crowd every other row out of the content budget.
		row := strings.Join(cells, " | ")
		if len(row) > 2*passageChars {
			row = truncateRunes(row, 2*passageChars) + " …"
		}
		r.emit(block{kind: blockRow, text: row, table: id, head: first})
		first = false

		if r.full() {
			return
		}
	}
}

// tableRows returns the rows of t itself, not of tables nested in its cells.
func tableRows(t *html.Node) []*html.Node {
	var rows []*html.Node
	for c := t.FirstChild; c != nil; c = c.NextSibling {
		if c.Type != html.ElementNode {
			continue
		}
		switch c.DataAtom {
		case atom.Tr:
			rows = append(rows, c)
		case atom.Thead, atom.Tbody, atom.Tfoot:
			for tr := c.FirstChild; tr != nil; tr = tr.NextSibling {
				if tr.Type == html.ElementNode && tr.DataAtom == atom.Tr {
					rows = append(rows, tr)
				}
			}
		}
	}
	return rows
}

// layoutTable reports whether a table lays out a page rather than holding
// data: one column only, a table nested in a cell, or a cell holding a
// whole passage. Such a table is read as ordinary text.
func layoutTable(t *html.Node) bool {
	maxCells := 0
	for _, tr := range tableRows(t) {
		cells := 0
		for c := tr.FirstChild; c != nil; c = c.NextSibling {
			if c.Type != html.ElementNode || (c.DataAtom != atom.Td && c.DataAtom != atom.Th) {
				continue
			}
			cells++
			if hasDescendant(c, atom.Table) || utf8.RuneCountInString(inlineText(c)) > 400 {
				return true
			}
		}
		maxCells = max(maxCells, cells)
	}
	return maxCells < 2
}

func hasDescendant(n *html.Node, a atom.Atom) bool {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && (c.DataAtom == a || hasDescendant(c, a)) {
			return true
		}
	}
	return false
}

func findChild(n *html.Node, a atom.Atom) *html.Node {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && c.DataAtom == a {
			return c
		}
	}
	return nil
}

// inlineText is n's visible text on one line.
func inlineText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		switch n.Type {
		case html.TextNode:
			b.WriteString(n.Data)
		case html.ElementNode:
			if skipElement(n) {
				return
			}
			if n.DataAtom == atom.Br || blockElements[n.DataAtom] || n.DataAtom == atom.Li {
				b.WriteByte(' ')
			}
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
			if blockElements[n.DataAtom] || n.DataAtom == atom.Li {
				b.WriteByte(' ')
			}
		default:
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
		}
	}
	walk(n)
	return strings.Join(strings.Fields(b.String()), " ")
}

var blockElements = map[atom.Atom]bool{
	atom.Address: true, atom.Article: true, atom.Aside: true, atom.Blockquote: true,
	atom.Caption: true, atom.Center: true, atom.Dd: true, atom.Details: true,
	atom.Dialog: true, atom.Div: true, atom.Dl: true, atom.Dt: true,
	atom.Fieldset: true, atom.Figcaption: true, atom.Figure: true, atom.Footer: true,
	atom.Form: true, atom.Header: true, atom.Hgroup: true, atom.Hr: true,
	atom.Legend: true, atom.Main: true, atom.Menu: true, atom.Nav: true,
	atom.Ol: true, atom.P: true, atom.Pre: true, atom.Section: true,
	atom.Summary: true, atom.Tbody: true, atom.Td: true, atom.Tfoot: true,
	atom.Th: true, atom.Thead: true, atom.Tr: true, atom.Ul: true,
}

var skippedElements = map[atom.Atom]bool{
	atom.Script: true, atom.Style: true, atom.Noscript: true, atom.Template: true,
	atom.Svg: true, atom.Math: true, atom.Iframe: true, atom.Object: true,
	atom.Embed: true, atom.Canvas: true, atom.Select: true, atom.Option: true,
	atom.Button: true, atom.Input: true, atom.Textarea: true, atom.Video: true,
	atom.Audio: true, atom.Img: true, atom.Picture: true, atom.Head: true,
}

// skipElement reports whether n and everything inside it is invisible or not
// text: scripts, media, form controls, and elements hidden from readers.
func skipElement(n *html.Node) bool {
	if skippedElements[n.DataAtom] {
		return true
	}
	if n.DataAtom == 0 && (n.Data == "svg" || n.Data == "math") {
		return true
	}

	for _, a := range n.Attr {
		switch a.Key {
		case "hidden":
			return true
		case "aria-hidden":
			if strings.EqualFold(a.Val, "true") {
				return true
			}
		case "style":
			style := strings.ReplaceAll(strings.ToLower(a.Val), " ", "")
			if strings.Contains(style, "display:none") || strings.Contains(style, "visibility:hidden") {
				return true
			}
		}
	}

	return false
}

// dedupeAdjacent drops a block that repeats the one before it, as menus and
// carousels often do.
func dedupeAdjacent(blocks []block) []block {
	out := make([]block, 0, len(blocks))
	for _, b := range blocks {
		if n := len(out); n > 0 && b.kind != blockRow && b.kind == out[n-1].kind && b.text == out[n-1].text {
			continue
		}
		out = append(out, b)
	}
	return out
}

// pageBody returns the page's body with its navigation chrome removed: nav,
// aside and footer anywhere, and header elements outside the main content.
// Forms stay, because ASP.NET pages wrap their whole content in one.
func pageBody(root *html.Node) *html.Node {
	body := findElement(root, atom.Body)
	if body == nil {
		return root
	}

	var chrome []*html.Node
	var walk func(n *html.Node, inMain bool)
	walk = func(n *html.Node, inMain bool) {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type != html.ElementNode {
				continue
			}
			switch c.DataAtom {
			case atom.Nav, atom.Aside, atom.Footer:
				chrome = append(chrome, c)
				continue
			case atom.Header:
				if !inMain {
					chrome = append(chrome, c)
					continue
				}
			}
			walk(c, inMain || c.DataAtom == atom.Main || c.DataAtom == atom.Article)
		}
	}
	walk(body, false)

	for _, n := range chrome {
		n.Parent.RemoveChild(n)
	}

	return body
}

func findElement(n *html.Node, a atom.Atom) *html.Node {
	if n.Type == html.ElementNode && n.DataAtom == a {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if found := findElement(c, a); found != nil {
			return found
		}
	}
	return nil
}
