package websearch

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
)

const resultLimit = 10

var userAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36"

type Result struct {
	Rank    int    `json:"rank"`
	Title   string `json:"title"`
	URL     string `json:"url"`
	Site    string `json:"site"`
	Snippet string `json:"snippet"`
}

type Response struct {
	Query   string   `json:"query"`
	Results []Result `json:"results"`
}

func Search(ctx context.Context, query string) (Response, error) {
	searchURL := "https://www.bing.com/search?q=" + url.QueryEscape(query)

	doc, err := fetchDocument(ctx, searchURL)
	if err != nil {
		return Response{}, err
	}

	return Response{Query: query, Results: extractResults(doc)}, nil
}

func extractResults(doc *goquery.Document) []Result {
	blocks := doc.Find("li.b_algo")

	var results []Result

	blocks.EachWithBreak(func(i int, block *goquery.Selection) bool {
		if i >= resultLimit {
			return false
		}

		link := block.Find("h2 a").First()
		if link.Length() == 0 {
			return true
		}

		title := strings.TrimSpace(link.Text())

		href, ok := link.Attr("href")
		if !ok {
			return true
		}

		href = decodeBingURL(href)
		snippet := strings.TrimSpace(block.Find(".b_caption p").First().Text())

		results = append(results, Result{
			Rank:    i + 1,
			Title:   title,
			URL:     href,
			Site:    getDomain(href),
			Snippet: snippet,
		})

		return true
	})

	return results
}

var cookieMu sync.Mutex

func fetchDocument(ctx context.Context, searchURL string) (*goquery.Document, error) {
	target, err := url.Parse(searchURL)
	if err != nil {
		return nil, err
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, searchURL, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "id-ID,id;q=0.9,en-US;q=0.8,en;q=0.7")

	client := &http.Client{Timeout: 15 * time.Second, Jar: jar}

	cookiePath, err := cookieFilePath()

	cookieMu.Lock()
	defer cookieMu.Unlock()

	if err == nil {
		loadCookies(cookiePath, target, jar)
	}

	resp, err := doWithRetry(client, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %s", resp.Status)
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, err
	}

	if cookiePath != "" {
		_ = saveCookies(cookiePath, target, jar)
	}

	if doc.Find("#turnstile-widget, .captcha_header").Length() > 0 {
		return nil, fmt.Errorf("diblokir sementara oleh Bing (captcha) - request terlalu sering, coba lagi beberapa menit lagi")
	}

	return doc, nil
}

func cookieFilePath() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}

	dir = filepath.Join(dir, "websearch-tool")

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	return filepath.Join(dir, "cookies.json"), nil
}

func loadCookies(path string, target *url.URL, jar *cookiejar.Jar) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}

	var cookies []*http.Cookie
	if err := json.Unmarshal(data, &cookies); err != nil {
		return
	}

	jar.SetCookies(target, cookies)
}

func saveCookies(path string, target *url.URL, jar *cookiejar.Jar) error {
	cookies := jar.Cookies(target)

	data, err := json.MarshalIndent(cookies, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(path, data, 0o644)
}

func doWithRetry(client *http.Client, req *http.Request) (*http.Response, error) {
	resp, err := client.Do(req)
	if err == nil || req.Context().Err() != nil {
		return resp, err
	}

	time.Sleep(500 * time.Millisecond)
	return client.Do(req)
}

func getDomain(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}

	host := u.Hostname()
	host = strings.TrimPrefix(host, "www.")

	return host
}

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

	if strings.HasPrefix(encoded, "a1") {
		encoded = strings.TrimPrefix(encoded, "a1")
	}

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
