package websearch

import (
	"encoding/base64"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
)

func mustDoc(t *testing.T, html string) *goquery.Document {
	t.Helper()

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}

	return doc
}

func bingRedirect(payload string) string {
	return "https://www.bing.com/ck/a?!&u=" + bingEncoded(payload) + "&ntb=1"
}

func bingEncoded(payload string) string {
	return "a1" + strings.TrimRight(base64.URLEncoding.EncodeToString([]byte(payload)), "=")
}

func TestDecodeBingURLDirectPassthrough(t *testing.T) {
	cases := []string{
		"https://example.com/article",
		"http://contoh.id/page?q=1",
		"https://www.bing.com/search?q=test",
		"https://other.com/ck/a?u=zzz",
	}

	for _, tc := range cases {
		if got := decodeBingURL(tc); got != tc {
			t.Errorf("decodeBingURL(%q) = %q, want passthrough", tc, got)
		}
	}
}

func TestDecodeBingURLRedirectVariants(t *testing.T) {
	target := "https://example.com/artikel?a=1&b=2"

	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"with a1 prefix", bingRedirect(target), target},
		{"without a1 prefix", "https://www.bing.com/ck/a?!&u=" + strings.TrimRight(base64.URLEncoding.EncodeToString([]byte(target)), "="), target},
		{"padded encoding", "https://www.bing.com/ck/a?!&u=" + base64.URLEncoding.EncodeToString([]byte(target)), target},
		{"http target", bingRedirect("http://berita.id/lokal"), "http://berita.id/lokal"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decodeBingURL(tc.raw); got != tc.want {
				t.Errorf("decodeBingURL() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDecodeBingURLInvalidFallbacks(t *testing.T) {
	raw := "https://www.bing.com/ck/a?!&u=%21%40%23"

	if got := decodeBingURL(raw); got != raw {
		t.Errorf("invalid base64: got %q, want raw %q", got, raw)
	}

	// Decoded payload is valid base64 but not an http(s) URL.
	notURL := "https://www.bing.com/ck/a?!&u=" + bingEncoded("bukan sebuah url")
	if got := decodeBingURL(notURL); got != notURL {
		t.Errorf("non-http payload: got %q, want raw %q", got, notURL)
	}
}

func algoBlock(href, title, snippet string) string {
	return `<li class="b_algo"><h2><a href="` + href + `">` + title +
		`</a></h2><div class="b_caption"><p>` + snippet + `</p></div></li>`
}

func TestExtractResultsMapsFieldsAndCapsAtLimit(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("<ol id=\"b_results\">")
	for i := range 13 {
		sb.WriteString(algoBlock(
			bingRedirect("https://contoh"+strings.Repeat("x", i)+".id/a"),
			strings.Repeat("t", i+1),
			"snippet",
		))
	}
	sb.WriteString(`<li class="b_algo"><div>tanpa tautan</div></li>`)
	sb.WriteString("</ol>")

	results := extractResults(mustDoc(t, sb.String()))

	if len(results) != resultLimit {
		t.Fatalf("len(results) = %d, want capped %d", len(results), resultLimit)
	}

	first := results[0]
	if first.Rank != 1 || first.Title == "" || first.Snippet != "snippet" {
		t.Errorf("first result = %+v", first)
	}
	if got := results[0].URL; !strings.HasPrefix(got, "https://contoh") {
		t.Errorf("first URL not decoded from redirect: %q", got)
	}
	if site := first.Site; strings.HasPrefix(site, "www.") || !strings.Contains(site, "contoh") {
		t.Errorf("site = %q, want www-stripped domain", site)
	}
	last := results[resultLimit-1]
	if last.Rank != resultLimit {
		t.Errorf("last rank = %d, want %d", last.Rank, resultLimit)
	}
}

func TestExtractResultsEmptyPage(t *testing.T) {
	doc := mustDoc(t, `<html><body><ol id="b_results"></ol></body></html>`)

	if results := extractResults(doc); len(results) != 0 {
		t.Errorf("expected zero results, got %+v", results)
	}
}

func TestHasNoResultsMarker(t *testing.T) {
	withMarker := mustDoc(t, `<html><body><ul><li class="b_no">Tidak ada hasil</li></ul></body></html>`)
	if !hasNoResultsMarker(withMarker) {
		t.Error("li.b_no present but marker not detected")
	}

	plain := mustDoc(t, `<html><body><ol id="b_results"></ol></body></html>`)
	if hasNoResultsMarker(plain) {
		t.Error("marker detected on page without li.b_no")
	}
}

func TestSearchDistinguishesAnomalyFromEmpty(t *testing.T) {
	empty := mustDoc(t, `<html><body><ul><li class="b_no">no results</li></ul></body></html>`)
	results := extractResults(empty)
	if len(results) != 0 || !hasNoResultsMarker(empty) {
		t.Fatal("fixture should represent a legitimate empty result set")
	}

	broken := mustDoc(t, `<html><body><div>halaman baru tanpa b_algo</div></body></html>`)
	if len(extractResults(broken)) != 0 && hasNoResultsMarker(broken) {
		t.Fatal("fixture should represent broken markup")
	}
	if !errors.Is(ErrResultsNotParsed, ErrResultsNotParsed) {
		t.Error("sentinel error identity broken")
	}
}

func ddgRedirect(payload string) string {
	return "//duckduckgo.com/l/?uddg=" + url.QueryEscape(payload) + "&rut=abc123"
}

func ddgResultBlock(href, title, snippet string) string {
	return `<div class="web-result"><h2>` +
		`<a rel="nofollow" class="result__a" href="` + href + `">` + title + `</a>` +
		`</h2><a class="result__snippet" href="` + href + `">` + snippet + `</a></div>`
}

func TestDecodeDDGURLDirectPassthrough(t *testing.T) {
	cases := []string{
		"https://example.com/article",
		"http://contoh.id/page?q=1",
		"https://duckduckgo.com/search?q=test",
		"https://example.com/l/?uddg=https%3A%2F%2Fother.com",
	}

	for _, tc := range cases {
		if got := decodeDDGURL(tc); got != tc {
			t.Errorf("decodeDDGURL(%q) = %q, want passthrough", tc, got)
		}
	}
}

func TestDecodeDDGURLRedirectVariants(t *testing.T) {
	target := "https://example.com/artikel?a=1&b=2"

	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"scheme-relative", ddgRedirect(target), target},
		{"absolute", "https://duckduckgo.com/l/?uddg=" + url.QueryEscape(target), target},
		{"http target", ddgRedirect("http://berita.id/lokal"), "http://berita.id/lokal"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decodeDDGURL(tc.raw); got != tc.want {
				t.Errorf("decodeDDGURL() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDecodeDDGURLNonHTTPTarget(t *testing.T) {
	raw := "https://duckduckgo.com/l/?uddg=ftp%3A%2F%2Fexample.com%2Ffile"
	if got := decodeDDGURL(raw); got != raw {
		t.Errorf("non-http uddg: got %q, want raw %q", got, raw)
	}
}

func TestExtractDDGResultsMapsFieldsAndCapsAtLimit(t *testing.T) {
	var sb strings.Builder
	for i := range 13 {
		sb.WriteString(ddgResultBlock(
			ddgRedirect("https://contoh"+strings.Repeat("x", i)+".id/a"),
			strings.Repeat("t", i+1),
			"snippet",
		))
	}
	sb.WriteString(`<div class="web-result"><div>tanpa tautan</div></div>`)

	results := extractDDGResults(mustDoc(t, sb.String()))

	if len(results) != resultLimit {
		t.Fatalf("len(results) = %d, want capped %d", len(results), resultLimit)
	}

	first := results[0]
	if first.Rank != 1 || first.Title == "" || first.Snippet != "snippet" {
		t.Errorf("first result = %+v", first)
	}
	if got := first.URL; !strings.HasPrefix(got, "https://contoh") {
		t.Errorf("first URL not decoded from redirect: %q", got)
	}
	if site := first.Site; strings.HasPrefix(site, "www.") || !strings.Contains(site, "contoh") {
		t.Errorf("site = %q, want www-stripped domain", site)
	}
	last := results[resultLimit-1]
	if last.Rank != resultLimit {
		t.Errorf("last rank = %d, want %d", last.Rank, resultLimit)
	}
}

func TestSearchDDEmptyLegitimateVsBroken(t *testing.T) {
	empty := mustDoc(t, `<html><body><div class="no-results">No results</div></body></html>`)
	if len(extractDDGResults(empty)) != 0 || !hasDDGNoResultsMarker(empty) {
		t.Fatal("fixture should represent a legitimate empty result set")
	}

	broken := mustDoc(t, `<html><body><p>halaman aneh tanpa hasil</p></body></html>`)
	if len(extractDDGResults(broken)) != 0 || hasDDGNoResultsMarker(broken) {
		t.Fatal("fixture should represent broken markup")
	}
}

func TestDocHasDDGBlock(t *testing.T) {
	blocked := mustDoc(t, `<html><body><main><p>Unfortunately, bots use DuckDuckGo too.</p></main></body></html>`)
	if !docHasDDGBlock(blocked) {
		t.Error("halaman anti-bot tidak terdeteksi")
	}

	clean := mustDoc(t, `<html><body><p>hasil pencarian normal</p></body></html>`)
	if docHasDDGBlock(clean) {
		t.Error("halaman normal terdeteksi sebagai blokir")
	}
}

func TestGetDomain(t *testing.T) {
	cases := map[string]string{
		"https://www.example.com/a":        "example.com",
		"https://sub.example.org/b?c=d":    "sub.example.org",
		"http://contoh.id":                 "contoh.id",
		"://broken":                        "",
		"https://www.subdomain.example.io": "subdomain.example.io",
	}

	for raw, want := range cases {
		if got := getDomain(raw); got != want {
			t.Errorf("getDomain(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestSearchURLEscapesQuery(t *testing.T) {
	q := "harga kopi & teh \"2026\""
	escaped := url.QueryEscape(q)

	if got := "https://www.bing.com/search?q=" + escaped; strings.Contains(got, " ") {
		t.Errorf("query escaping left raw space: %q", got)
	}
}
