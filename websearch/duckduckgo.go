package websearch

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/PuerkitoBio/goquery"
)

// DuckDuckGoEndpoint is DuckDuckGo's HTML results endpoint. Override it only
// in tests.
var DuckDuckGoEndpoint = "https://html.duckduckgo.com/html"

// DuckDuckGoLiteEndpoint is DuckDuckGo's text-only endpoint, read when the
// HTML endpoint's markup cannot be parsed. Override it only in tests.
var DuckDuckGoLiteEndpoint = "https://lite.duckduckgo.com/lite/"

// DuckDuckGoRegion biases DuckDuckGo toward Indonesian results through its
// kl=<region> parameter, whatever country the request comes from.
var DuckDuckGoRegion = "id-id"

// searchDuckDuckGo reads the HTML endpoint and falls back to the text-only
// one when the HTML markup no longer parses or the endpoint fails outright.
// A challenge on either is final: both sit behind the same anti-bot check,
// and asking again only prolongs the block.
func searchDuckDuckGo(ctx context.Context, query string) ([]Result, error) {
	results, err := searchDuckDuckGoHTML(ctx, query)
	if err == nil || errors.Is(err, ErrBlocked) || ctx.Err() != nil {
		return results, err
	}

	if liteResults, liteErr := searchDuckDuckGoLite(ctx, query); liteErr == nil {
		return liteResults, nil
	} else if errors.Is(liteErr, ErrBlocked) {
		return nil, liteErr
	}

	return nil, err
}

func searchDuckDuckGoHTML(ctx context.Context, query string) ([]Result, error) {
	searchURL := strings.TrimRight(DuckDuckGoEndpoint, "/") + "/?q=" + url.QueryEscape(query) +
		"&kl=" + url.QueryEscape(DuckDuckGoRegion)

	doc, err := fetchDuckDuckGo(ctx, searchURL)
	if err != nil {
		return nil, err
	}

	results := extractDDGResults(doc)
	if len(results) == 0 && !hasDDGNoResultsMarker(doc) {
		return nil, ErrResultsNotParsed
	}

	return results, nil
}

func searchDuckDuckGoLite(ctx context.Context, query string) ([]Result, error) {
	searchURL := DuckDuckGoLiteEndpoint + "?q=" + url.QueryEscape(query) +
		"&kl=" + url.QueryEscape(DuckDuckGoRegion)

	doc, err := fetchDuckDuckGo(ctx, searchURL)
	if err != nil {
		return nil, err
	}

	results := extractDDGLiteResults(doc)
	if len(results) == 0 && !hasDDGLiteNoResultsMarker(doc) {
		return nil, ErrResultsNotParsed
	}

	return results, nil
}

func fetchDuckDuckGo(ctx context.Context, searchURL string) (*goquery.Document, error) {
	p, err := fetch(ctx, searchURL, requestOptions{browser: true, cookieFile: "cookies-ddg.json"})
	if err != nil {
		return nil, err
	}

	// DuckDuckGo answers a suspected bot with HTTP 202 and a challenge page;
	// 403 and 429 mean the same thing.
	switch p.status {
	case http.StatusOK:
	case http.StatusAccepted, http.StatusForbidden, http.StatusTooManyRequests:
		return nil, ErrBlocked
	default:
		return nil, statusError(p.status)
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(p.body))
	if err != nil {
		return nil, err
	}

	if docHasDDGBlock(doc) {
		return nil, ErrBlocked
	}

	return doc, nil
}

// docHasDDGBlock reports whether the page is DuckDuckGo's anti-bot challenge
// rather than a results page.
func docHasDDGBlock(doc *goquery.Document) bool {
	if doc.Find("#challenge-form, .anomaly-modal__mask, form[action*='anomaly']").Length() > 0 {
		return true
	}

	text := strings.ToLower(doc.Find("body").Text())

	return strings.Contains(text, "bots use duckduckgo") ||
		strings.Contains(text, "unusual traffic")
}

// hasDDGNoResultsMarker reports whether the page carries DuckDuckGo's
// standard "no results" notice, making an empty result set legitimate.
func hasDDGNoResultsMarker(doc *goquery.Document) bool {
	return doc.Find("div.no-results").Length() > 0
}

func extractDDGResults(doc *goquery.Document) []Result {
	var results []Result

	doc.Find("div.web-result").EachWithBreak(func(_ int, block *goquery.Selection) bool {
		if len(results) >= resultLimit {
			return false
		}
		if block.HasClass("result--ad") {
			return true
		}

		link := block.Find("a.result__a").First()
		if link.Length() == 0 {
			return true
		}

		href, ok := link.Attr("href")
		if !ok {
			return true
		}

		href = decodeDDGURL(href)
		if !isWebURL(href) {
			return true
		}

		results = append(results, Result{
			Rank:    len(results) + 1,
			Title:   cleanText(link.Text()),
			URL:     href,
			Site:    getDomain(href),
			Snippet: cleanText(block.Find(".result__snippet").First().Text()),
		})

		return true
	})

	return results
}

// extractDDGLiteResults reads the text-only page, a table in which each
// result is a row holding the link, followed by rows holding its snippet and
// its address. Sponsored rows are skipped.
func extractDDGLiteResults(doc *goquery.Document) []Result {
	var results []Result

	doc.Find("a.result-link").EachWithBreak(func(_ int, link *goquery.Selection) bool {
		if len(results) >= resultLimit {
			return false
		}

		row := link.Closest("tr")
		if row.HasClass("result-sponsored") {
			return true
		}

		href, ok := link.Attr("href")
		if !ok {
			return true
		}

		href = decodeDDGURL(href)
		if !isWebURL(href) {
			return true
		}

		snippet := ""
		for next := row.Next(); next.Length() > 0; next = next.Next() {
			if next.Find("a.result-link").Length() > 0 {
				break
			}
			if text := cleanText(next.Find(".result-snippet").First().Text()); text != "" {
				snippet = text
				break
			}
		}

		results = append(results, Result{
			Rank:    len(results) + 1,
			Title:   cleanText(link.Text()),
			URL:     href,
			Site:    getDomain(href),
			Snippet: snippet,
		})

		return true
	})

	return results
}

func hasDDGLiteNoResultsMarker(doc *goquery.Document) bool {
	if doc.Find(".no-results").Length() > 0 {
		return true
	}

	text := strings.ToLower(doc.Find("body").Text())

	return strings.Contains(text, "no results") || strings.Contains(text, "tidak ada hasil")
}

// decodeDDGURL unwraps DuckDuckGo's //duckduckgo.com/l/?uddg=<escaped>
// redirect links into their target URL; anything else passes through
// unchanged.
func decodeDDGURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}

	host := strings.ToLower(u.Hostname())
	if !strings.Contains(host, "duckduckgo.com") || u.Path != "/l/" {
		return rawURL
	}

	target := u.Query().Get("uddg")
	if strings.HasPrefix(target, "http://") ||
		strings.HasPrefix(target, "https:") {
		return target
	}

	return rawURL
}

func isWebURL(raw string) bool {
	return strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://")
}
