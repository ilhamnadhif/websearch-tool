package fetchpage

import (
	"math"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/ilhamnadhif/websearch-tool/internal/textmatch"
)

const (
	passageChars = 700 // a passage closes once it holds this much text
	passageRows  = 8   // or this many rows of one table
	gapMarker    = "…" // marks text left out between two chosen passages
)

// passage is a run of consecutive blocks that focus selection keeps or drops
// as a whole.
type passage struct {
	start, end int // blocks[start:end]
	heading    int // the section heading it sits under, when it does not start with it; else -1
	headRow    int // its table's header row, when it starts after it; else -1
	score      float64
}

// present renders doc as Result.Content: its structured facts first, then as
// much of its text as fits in Options.MaxChars -- the page's opening, or,
// with a focus, the passages that best match it.
func present(doc *document, opts Options) string {
	limit := opts.MaxChars
	if limit <= 0 {
		limit = maxContentChars
	}

	var parts []string
	used := 0

	if len(doc.facts) > 0 {
		facts := truncateRunes(strings.Join(doc.facts, "\n"), limit/3)
		parts = append(parts, facts)
		used += utf8.RuneCountInString(facts) + 1
	}

	if body := selectText(doc.blocks, opts.Focus, limit-used); body != "" {
		parts = append(parts, body)
	}

	return truncateRunes(strings.Join(parts, "\n"), limit)
}

func selectText(blocks []block, focus string, budget int) string {
	if budget <= 0 || len(blocks) == 0 {
		return ""
	}
	if blocksLen(blocks) <= budget {
		return blocksText(blocks)
	}

	if terms := focusTerms(focus); len(terms) > 0 {
		if text, ok := selectPassages(blocks, terms, budget); ok {
			return text
		}
	}

	return leadingText(blocks, budget)
}

// focusTerms returns the words of focus worth looking for. Unlike a search
// query's relevance check, generic words stay: "harga" is exactly what
// finds the price table on a long page.
func focusTerms(focus string) []string {
	seen := map[string]bool{}

	var terms []string
	for _, w := range textmatch.Words(focus) {
		if len(w) < 2 || seen[w] || textmatch.IsStopword(w) {
			continue
		}
		seen[w] = true
		terms = append(terms, w)
	}

	return terms
}

// leadingText returns the opening blocks that fit in budget, the last one
// cut short when enough room is left to make it worth reading.
func leadingText(blocks []block, budget int) string {
	var lines []string
	used := 0

	for _, b := range blocks {
		line := b.String()
		n := utf8.RuneCountInString(line) + 1

		if used+n > budget {
			if room := budget - used - 1; len(lines) == 0 || room >= 80 {
				lines = append(lines, truncateRunes(line, room))
			}
			break
		}

		lines = append(lines, line)
		used += n
	}

	return strings.Join(lines, "\n")
}

// selectPassages scores each passage by the focus terms it contains, weighted
// by how rare each term is on the page, keeps the best ones that fit in
// budget, and fills what room is left with the rest of the page in order.
// The result reads in page order. A gap marker shows where text was left
// out, and a passage that starts mid-section or mid-table brings its heading
// or its table's header row along, so a row of numbers still says what it
// means.
func selectPassages(blocks []block, terms []string, budget int) (string, bool) {
	passages := splitPassages(blocks)

	texts := make([]textmatch.Text, len(passages))
	for i, p := range passages {
		texts[i] = textmatch.NewText(blocksText(blocks[p.start:p.end]))
	}

	df := map[string]int{}
	for _, t := range terms {
		for _, text := range texts {
			if text.Has(t) {
				df[t]++
			}
		}
	}

	n := float64(len(passages))
	best := 0.0
	for i := range passages {
		score := 0.0
		for _, t := range terms {
			if count := texts[i].Count(t); count > 0 {
				score += math.Log(1+n/float64(df[t])) * (1 + 0.25*float64(min(count-1, 4)))
			}
		}
		passages[i].score = score
		best = max(best, score)
	}
	if best == 0 {
		return "", false
	}

	chosen := make([]bool, len(passages))
	used := 0
	matched := false

	// The opening passage usually holds the title, the date and the gist;
	// keep it when it is short.
	if cost := passageCost(blocks, passages[0]); cost <= budget/5 {
		chosen[0] = true
		used += cost
	}

	order := make([]int, len(passages))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int {
		switch {
		case passages[a].score > passages[b].score:
			return -1
		case passages[a].score < passages[b].score:
			return 1
		}
		return 0
	})

	for _, i := range order {
		if passages[i].score == 0 {
			break
		}
		if chosen[i] {
			continue
		}
		if cost := passageCost(blocks, passages[i]); used+cost <= budget {
			chosen[i] = true
			used += cost
			matched = true
		}
	}

	// No matching passage fits: the opening chosen above would be all that
	// comes back, less than the page's plain opening would give.
	if !matched && !(chosen[0] && passages[0].score > 0) {
		return "", false
	}

	// Room left after the matching passages goes to the rest of the page in
	// order: a reader who worded the question differently from the page
	// still gets its context, and the matching passages are already in.
	for i := range passages {
		if chosen[i] {
			continue
		}
		if cost := passageCost(blocks, passages[i]); used+cost <= budget {
			chosen[i] = true
			used += cost
		}
	}

	included := map[int]bool{}
	for i, p := range passages {
		if chosen[i] {
			for j := p.start; j < p.end; j++ {
				included[j] = true
			}
		}
	}
	if len(included) == 0 {
		return "", false
	}

	var lines []string
	for i, p := range passages {
		if !chosen[i] {
			continue
		}

		if i == 0 || !chosen[i-1] {
			if i > 0 {
				lines = append(lines, gapMarker)
			}
			for _, ctx := range []int{p.heading, p.headRow} {
				if ctx >= 0 && !included[ctx] {
					lines = append(lines, blocks[ctx].String())
				}
			}
		}

		for _, b := range blocks[p.start:p.end] {
			lines = append(lines, b.String())
		}
	}
	if !chosen[len(passages)-1] {
		lines = append(lines, gapMarker)
	}

	return strings.Join(lines, "\n"), true
}

// passageCost is what including p takes from the budget: its text, its
// context lines, and a gap marker.
func passageCost(blocks []block, p passage) int {
	cost := blocksLen(blocks[p.start:p.end]) + 2
	for _, ctx := range []int{p.heading, p.headRow} {
		if ctx >= 0 {
			cost += utf8.RuneCountInString(blocks[ctx].String()) + 1
		}
	}
	return cost
}

// splitPassages cuts blocks into passages: at every heading, where a table
// starts or ends, every passageRows rows of a table, and otherwise once a
// passage holds passageChars characters.
func splitPassages(blocks []block) []passage {
	var out []passage

	lastHeading := -1
	tableHead := map[int]int{}
	cur := passage{heading: -1, headRow: -1}
	size, rows := 0, 0

	cut := func(i int) {
		if i > cur.start {
			cur.end = i
			out = append(out, cur)
		}
		cur = passage{start: i, heading: lastHeading, headRow: -1}
		size, rows = 0, 0
	}

	for i, b := range blocks {
		switch b.kind {
		case blockHeading:
			cut(i)
			lastHeading = i
			cur.heading = -1
		case blockRow:
			if b.head {
				tableHead[b.table] = i
			}
			continuesTable := i > cur.start && blocks[i-1].kind == blockRow && blocks[i-1].table == b.table
			if (!continuesTable && i > cur.start) || rows >= passageRows {
				cut(i)
			}
			if i == cur.start && !b.head {
				if head, ok := tableHead[b.table]; ok {
					cur.headRow = head
				}
			}
			rows++
		default:
			if i > cur.start && (blocks[i-1].kind == blockRow || size >= passageChars) {
				cut(i)
			}
		}

		size += utf8.RuneCountInString(b.String()) + 1
	}
	cut(len(blocks))

	return out
}

// truncateRunes cuts s to at most n characters without splitting a UTF-8
// sequence.
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}

	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

// truncateContent cuts text to the default content length.
func truncateContent(text string) string {
	return truncateRunes(text, maxContentChars)
}
