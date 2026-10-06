package websearch

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/PuerkitoBio/goquery"
)

// SearchEndpoint is the Bing results URL, appended with "?q=<escaped query>"
// and BingMarketParams. Override it only in tests.
var SearchEndpoint = "https://www.bing.com/search"

// BingMarketParams pins Bing (web and news) to its Indonesian market wherever
// the request comes from, so a VPS or VPN abroad still gets Indonesian
// results. It is appended to the query string verbatim; change it if your
// audience is elsewhere.
var BingMarketParams = "mkt=id-ID&setlang=id&cc=ID"

var bingSnippetSelectors = []string{
	".b_caption p",
	"p.b_lineclamp4",
	"p.b_lineclamp3",
	"p.b_lineclamp2",
	"p.b_lineclamp1",
	".b_algoSlug",
	"div.b_snippet",
}

func searchBing(ctx context.Context, query string) ([]Result, error) {
	searchURL := SearchEndpoint + "?q=" + url.QueryEscape(query) + "&" + BingMarketParams

	p, err := fetch(ctx, searchURL, requestOptions{browser: true, cookieFile: "cookies.json"})
	if err != nil {
		return nil, err
	}

	if bingChallenged(p) {
		return nil, ErrBlocked
	}
	if p.status != http.StatusOK {
		return nil, statusError(p.status)
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(p.body))
	if err != nil {
		return nil, err
	}

	if doc.Find("#turnstile-widget, .captcha_header, #b_captcha").Length() > 0 {
		return nil, ErrBlocked
	}

	results := extractResults(doc)
	if len(results) == 0 && !hasNoResultsMarker(doc) {
		return nil, ErrResultsNotParsed
	}

	// An empty page with Bing's "no results" notice is not proof that the
	// web has nothing: Bing serves that same page to IPs it distrusts. The
	// chain treats it as a miss and asks the next engine.
	return results, nil
}

// bingChallenged reports whether Bing answered with its human-verification
// flow instead of results.
func bingChallenged(p *page) bool {
	if p.status == http.StatusTooManyRequests {
		return true
	}
	if p.url != nil && strings.Contains(p.url.Path, "/turing/captcha") {
		return true
	}
	return false
}

func extractResults(doc *goquery.Document) []Result {
	var results []Result

	doc.Find("li.b_algo").EachWithBreak(func(_ int, block *goquery.Selection) bool {
		if len(results) >= resultLimit {
			return false
		}

		link := block.Find("h2 a").First()
		if link.Length() == 0 {
			return true
		}

		href, ok := link.Attr("href")
		if !ok {
			return true
		}

		href = decodeBingURL(href)
		if !isWebURL(href) {
			return true
		}

		results = append(results, Result{
			Rank:    len(results) + 1,
			Title:   cleanText(link.Text()),
			URL:     href,
			Site:    getDomain(href),
			Snippet: bingSnippet(block),
		})

		return true
	})

	return results
}

func bingSnippet(block *goquery.Selection) string {
	for _, selector := range bingSnippetSelectors {
		if text := cleanText(block.Find(selector).First().Text()); text != "" {
			return text
		}
	}
	return ""
}

// hasNoResultsMarker reports whether the page carries Bing's "no results"
// notice (li.b_no), which tells an empty page apart from changed markup.
func hasNoResultsMarker(doc *goquery.Document) bool {
	return doc.Find("li.b_no").Length() > 0
}

// decodeBingURL unwraps Bing's /ck/a?u=a1<base64url> click-tracking links
// into their target; anything else passes through unchanged.
func decodeBingURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}

	if !strings.Contains(u.Host, "bing.com") ||
		!strings.HasPrefix(u.Path, "/ck/a") {
		return rawURL
	}

	encoded := u.Query().Get("u")
	if encoded == "" {
		return rawURL
	}

	encoded = strings.TrimPrefix(encoded, "a1")

	switch len(encoded) % 4 {
	case 2:
		encoded += "=="
	case 3:
		encoded += "="
	}

	decoded, err := base64.URLEncoding.DecodeString(encoded)
	if err != nil {
		decoded, err = base64.RawURLEncoding.DecodeString(
			strings.TrimRight(encoded, "="),
		)
		if err != nil {
			return rawURL
		}
	}

	result := string(decoded)

	if strings.HasPrefix(result, "http://") ||
		strings.HasPrefix(result, "https://") {
		return result
	}

	return rawURL
}

func statusError(status int) error {
	return fmt.Errorf("status %d %s", status, http.StatusText(status))
}
