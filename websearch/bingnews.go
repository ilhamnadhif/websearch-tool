package websearch

import (
	"bytes"
	"context"
	"encoding/xml"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// BingNewsEndpoint is Bing News' search URL. Search asks it for RSS, which
// Bing serves to datacenter IPs that its web results page turns away. Each
// item is a dated article with the publisher's own URL. Override it only in
// tests.
var BingNewsEndpoint = "https://www.bing.com/news/search"

type bingNewsFeed struct {
	Items []struct {
		Title       string `xml:"title"`
		Link        string `xml:"link"`
		Description string `xml:"description"`
		PubDate     string `xml:"pubDate"`
		Source      string `xml:"Source"`
	} `xml:"channel>item"`
}

func searchBingNews(ctx context.Context, query string) ([]Result, error) {
	searchURL := BingNewsEndpoint + "?format=rss&q=" + url.QueryEscape(query) + "&" + BingMarketParams

	p, err := fetch(ctx, searchURL, requestOptions{
		browser: true,
		header:  http.Header{"Accept": {"application/rss+xml, application/xml;q=0.9, */*;q=0.5"}},
	})
	if err != nil {
		return nil, err
	}

	if bingChallenged(p) {
		return nil, ErrBlocked
	}
	if p.status != http.StatusOK {
		return nil, statusError(p.status)
	}

	return parseBingNews(p.body)
}

func parseBingNews(body []byte) ([]Result, error) {
	if !bytes.Contains(body[:min(len(body), 512)], []byte("<rss")) {
		if bytes.Contains(body, []byte("captcha")) {
			return nil, ErrBlocked
		}
		return nil, ErrResultsNotParsed
	}

	var feed bingNewsFeed
	if err := xml.Unmarshal(body, &feed); err != nil {
		return nil, ErrResultsNotParsed
	}

	var results []Result
	for _, item := range feed.Items {
		if len(results) >= resultLimit {
			break
		}

		link := unwrapBingNewsLink(strings.TrimSpace(item.Link))
		if !isWebURL(link) {
			continue
		}

		results = append(results, Result{
			Rank:      len(results) + 1,
			Title:     cleanText(item.Title),
			URL:       link,
			Site:      getDomain(link),
			Snippet:   cleanText(item.Description),
			Published: parseRSSDate(item.PubDate),
		})
	}

	return results, nil
}

// unwrapBingNewsLink turns Bing's apiclick.aspx?...&url=<escaped> tracking
// link into the article's own URL.
func unwrapBingNewsLink(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}

	if strings.Contains(u.Host, "bing.com") {
		if target := u.Query().Get("url"); isWebURL(target) {
			return target
		}
	}

	return raw
}

func parseRSSDate(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	for _, layout := range []string{time.RFC1123, time.RFC1123Z, time.RFC822, time.RFC822Z, time.RFC3339} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}
