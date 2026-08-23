// Package fetchpage downloads a web page and extracts its title and main
// text content. It only fetches http/https URLs and refuses to connect to
// private, loopback, or link-local addresses. The destination IP is
// validated at dial time — including on every redirect hop, because
// redirects reuse the same guarded Transport — so DNS rebinding cannot
// bypass the check. Bodies are capped at maxBodyBytes. Use package
// websearch to find candidate URLs to fetch.
package fetchpage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"codeberg.org/readeck/go-readability/v2"
	"github.com/PuerkitoBio/goquery"
)

const (
	maxContentChars  = 4000
	minReadableChars = 200
	maxURLsPerCall   = 5
	maxBodyBytes     = 5 << 20 // 5 MB; pages larger than this are rejected before parsing
)

var userAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36"

var whitespaceRe = regexp.MustCompile(`\s+`)

var safeHTTPClient = &http.Client{
	Timeout: 10 * time.Second,
	Transport: &http.Transport{
		DialContext: safeDialContext,
	},
}

var safeDialer = &net.Dialer{Timeout: 10 * time.Second}

func isBlockedIP(ip net.IP) bool {
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() ||
		ip.IsMulticast()
}

func safeDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}

	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}

	for _, ip := range ips {
		if !isBlockedIP(ip) {
			return safeDialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		}
	}

	return nil, fmt.Errorf("%s mengarah ke jaringan privat/internal - diblokir", host)
}

// Result is the outcome of fetching one URL. If Error is non-empty, the
// fetch failed and Title/Content are left empty.
type Result struct {
	URL     string `json:"url"`
	Title   string `json:"title,omitempty"`
	Content string `json:"content,omitempty"`
	Error   string `json:"error,omitempty"`
}

// FetchMany fetches multiple URLs concurrently (one goroutine per URL) and
// returns one Result per URL, in the same order as urls. Requests are capped
// at 5 URLs per call; any beyond that are dropped. ctx controls cancellation
// and timeout for all fetches.
func FetchMany(ctx context.Context, urls []string) []Result {
	if len(urls) > maxURLsPerCall {
		urls = urls[:maxURLsPerCall]
	}

	results := make([]Result, len(urls))

	var wg sync.WaitGroup

	for i, pageURL := range urls {
		wg.Add(1)

		go func(i int, pageURL string) {
			defer wg.Done()

			results[i].URL = pageURL

			title, content, err := Fetch(ctx, pageURL)
			if err != nil {
				results[i].Error = err.Error()
				return
			}

			results[i].Title = title
			results[i].Content = content
		}(i, pageURL)
	}

	wg.Wait()

	return results
}

// Fetch downloads pageURL and extracts its title and main text content. It
// tries readability-style extraction first (suited to articles and blog
// posts) and falls back to a manual tag-stripping extraction when that
// yields too little text, e.g. on product listing pages. Content is
// truncated to 4000 characters. Bodies larger than maxBodyBytes are
// rejected instead of being read into memory.
//
// Both http and https URLs are fetched on purpose: many Indonesian sources
// are still http-only, and fetched content is transient (model context
// only) — it is never persisted or executed. The SSRF guard applies to both
// schemes equally.
func Fetch(ctx context.Context, pageURL string) (title, content string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return "", "", err
	}

	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return "", "", fmt.Errorf("skema URL tidak didukung: %s", req.URL.Scheme)
	}

	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "id-ID,id;q=0.9,en-US;q=0.8,en;q=0.7")

	resp, err := doWithRetry(safeHTTPClient, req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("status %s", resp.Status)
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType != "" && !strings.Contains(contentType, "html") {
		return "", "", fmt.Errorf("konten bukan HTML (%s)", contentType)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return "", "", err
	}
	if int64(len(body)) > maxBodyBytes {
		return "", "", fmt.Errorf("konten terlalu besar (> %d bytes)", maxBodyBytes)
	}

	if t, c, ok := extractReadable(body, req.URL); ok {
		return t, truncateContent(c), nil
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}

	fallbackTitle := strings.TrimSpace(doc.Find("title").First().Text())

	return fallbackTitle, truncateContent(extractCleanText(doc)), nil
}

func extractReadable(body []byte, pageURL *url.URL) (title, content string, ok bool) {
	article, err := readability.FromReader(bytes.NewReader(body), pageURL)
	if err != nil || article.Node == nil {
		return "", "", false
	}

	var buf strings.Builder
	if err := article.RenderText(&buf); err != nil {
		return "", "", false
	}

	text := whitespaceRe.ReplaceAllString(buf.String(), " ")
	text = strings.TrimSpace(text)

	if len(text) < minReadableChars {
		return "", "", false
	}

	return article.Title(), text, true
}

func extractCleanText(doc *goquery.Document) string {
	doc.Find("script, style, noscript, nav, header, footer, aside, form, iframe, svg").Remove()

	text := doc.Find("body").Text()
	text = whitespaceRe.ReplaceAllString(text, " ")

	return strings.TrimSpace(text)
}

func truncateContent(text string) string {
	if len(text) <= maxContentChars {
		return text
	}

	n := 0
	for i := range text {
		if n == maxContentChars {
			return text[:i]
		}
		n++
	}

	return text
}

func doWithRetry(client *http.Client, req *http.Request) (*http.Response, error) {
	resp, err := client.Do(req)
	if err == nil || req.Context().Err() != nil {
		return resp, err
	}

	select {
	case <-time.After(500 * time.Millisecond):
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
	return client.Do(req)
}
