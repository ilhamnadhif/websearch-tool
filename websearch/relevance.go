package websearch

import (
	"net/url"
	"strings"

	"github.com/ilhamnadhif/websearch-tool/internal/textmatch"
)

// relevanceWindow is how many of an engine's top results the relevance check
// reads. Decoy pages fill the top of the list, so the top is where to look.
const relevanceWindow = 5

var searchOperators = []string{"site:", "inurl:", "intitle:", "intext:", "filetype:", "ext:"}

type term struct {
	word   string
	weight float64
}

// queryTerms returns the words of query that a result about it is expected
// to mention, each with its weight. ok is false when the query uses a script
// other than Latin: word matching says nothing useful there, so the check is
// skipped.
func queryTerms(query string) (terms []term, ok bool) {
	seen := map[string]bool{}

	for _, field := range strings.Fields(query) {
		lower := strings.ToLower(field)
		if strings.HasPrefix(lower, "-") || hasOperator(lower) {
			continue
		}

		for _, word := range textmatch.Words(field) {
			if !textmatch.IsLatin(word) {
				return nil, false
			}
			if len(word) < 2 || textmatch.IsNumber(word) || seen[word] || textmatch.IsStopword(word) {
				continue
			}

			seen[word] = true

			// Generic words count half: a decoy that answers only the first
			// word of "jadwal krl bogor jakarta kota" with prayer timetables
			// for Jakarta matches three of its five words, all generic ones.
			weight := 1.0
			if textmatch.IsGeneric(word) {
				weight = 0.5
			}
			terms = append(terms, term{word: word, weight: weight})
		}
	}

	return terms, true
}

func hasOperator(field string) bool {
	for _, op := range searchOperators {
		if strings.HasPrefix(field, op) {
			return true
		}
	}
	return false
}

// filterRelevant reports whether results are about query at all, and drops
// the individual results that are not.
//
// Scraped engines sometimes answer suspected bots with links that have
// nothing to do with the query: random pages, or pages about only its first
// word ("siapa menteri keuangan" answered with dictionary entries for
// "siapa"). Such a page looks like a normal answer, so it is judged on
// content. A real answer puts at least one result near the top that covers
// half the query's weighted words in its title, snippet, or address; results
// covering less than a third are dropped from an accepted set.
//
// strict adds a second test for the engine known to serve those decoys
// (Bing): the query's specific words must appear in the top results, at most
// one in three missing. "resep rendang padang" answered with generic recipe
// sites passes the first test, because one of them happens to mention
// "masakan padang", but none mentions "rendang". The other engines stay on
// the first test alone, so a query worded unlike its answers ("bikin" where
// pages say "pembuatan") still finds them there.
func filterRelevant(query string, results []Result, strict bool) ([]Result, bool) {
	terms, ok := queryTerms(query)
	if !ok || len(terms) == 0 || len(results) == 0 {
		return results, true
	}

	total := 0.0
	for _, t := range terms {
		total += t.weight
	}

	texts := make([]textmatch.Text, len(results))
	scores := make([]float64, len(results))
	for i, r := range results {
		texts[i] = newResultText(r)
		scores[i] = coverage(texts[i], terms) / total
	}

	window := min(len(results), relevanceWindow)

	best := 0.0
	for _, score := range scores[:window] {
		best = max(best, score)
	}
	if best < 0.5 {
		return nil, false
	}

	if strict && missingSpecificTerms(terms, texts[:window]) > allowedMisses(terms) {
		return nil, false
	}

	kept := make([]Result, 0, len(results))
	for i, r := range results {
		if scores[i] >= 1.0/3 {
			r.Rank = len(kept) + 1
			kept = append(kept, r)
		}
	}

	return kept, true
}

// missingSpecificTerms counts the full-weight terms that none of texts
// mentions.
func missingSpecificTerms(terms []term, texts []textmatch.Text) int {
	missing := 0
	for _, t := range terms {
		if t.weight < 1 {
			continue
		}

		found := false
		for _, text := range texts {
			if text.Has(t.word) {
				found = true
				break
			}
		}
		if !found {
			missing++
		}
	}
	return missing
}

// allowedMisses tolerates one missing specific word per three beyond the
// first: none for up to three, one for four to six.
func allowedMisses(terms []term) int {
	specific := 0
	for _, t := range terms {
		if t.weight >= 1 {
			specific++
		}
	}
	return max(0, (specific-1)/3)
}

func newResultText(r Result) textmatch.Text {
	return textmatch.NewText(r.Title + " " + r.Snippet + " " + addressWords(r.URL))
}

func coverage(text textmatch.Text, terms []term) float64 {
	sum := 0.0
	for _, t := range terms {
		if text.Has(t.word) {
			sum += t.weight
		}
	}
	return sum
}

func addressWords(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}

	path, err := url.PathUnescape(u.EscapedPath())
	if err != nil {
		path = u.Path
	}

	return u.Hostname() + " " + path
}
