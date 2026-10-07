// Package fetchpage downloads a web page and extracts its title and readable
// text. It only fetches http/https URLs and refuses to connect to private,
// loopback, or link-local addresses. The destination IP is validated at dial
// time -- including on every redirect hop, because redirects reuse the same
// guarded Transport -- so DNS rebinding cannot bypass the check. Use package
// websearch to find candidate URLs to fetch.
//
// Content keeps the page's structure in plain text: "## " starts a heading,
// "- " a list item, and a table row's cells are joined by " | ". Facts the
// page states as structured data (a product's price, an office's opening
// hours) come first. Pass Options.Focus -- the question being answered --
// to get the parts of a long page that answer it instead of its opening.
//
// Besides HTML, fetchpage reads PDFs (through Poppler's pdftotext, see
// PDFToText) and plain text, MSN articles through MSN's content API, and
// Wikipedia articles through Wikipedia's API.
package fetchpage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"codeberg.org/readeck/go-readability/v2"
	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

const (
	maxContentChars  = 4000
	minReadableChars = 200
	minPageChars     = 120 // less visible text than this is a JavaScript shell
	maxURLsPerCall   = 5
	maxBodyBytes     = 5 << 20  // HTML and text; larger pages are rejected before parsing
	maxPDFBytes      = 15 << 20 // regulations and reports run larger than web pages
	cacheCapacity    = 64
)

// How Result.Content was obtained.
const (
	SourceArticle   = "article"   // the page's main article
	SourcePage      = "page"      // the page's whole text (listings, tables, forms)
	SourceSummary   = "summary"   // only the page's description and structured data: its text needs JavaScript
	SourceWikipedia = "wikipedia" // Wikipedia's API
	SourceMSN       = "msn"       // MSN's content API
	SourcePDF       = "pdf"       // a PDF's text layer
	SourceText      = "text"      // a text/plain response
)

var (
	// ErrBlocked means the site refused the request as automated (an
	// anti-bot challenge or a rate limit), even after retrying under
	// HonestUserAgent. Read another source.
	ErrBlocked = errors.New("situs menolak permintaan otomatis (anti-bot)")

	// ErrNoContent means the page has no text to read without running its
	// JavaScript, and no description either.
	ErrNoContent = errors.New("halaman tidak berisi teks yang bisa dibaca (kemungkinan butuh JavaScript)")

	// ErrPDFUnsupported means a PDF arrived and no converter is available
	// (see PDFToText).
	ErrPDFUnsupported = errors.New("PDF tidak bisa dibaca di sini (pdftotext tidak tersedia)")
)

var browserUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36"

// HonestUserAgent identifies the client in the second request sent after a
// site refused a browser-like one, and in every request to Wikipedia's API.
// Set it to your application's name and a contact address, e.g.
// "MyBot/1.0 (+https://example.com; ops@example.com)".
var HonestUserAgent = "websearch-tool/1.3 (+https://github.com/ilhamnadhif/websearch-tool)"

// CacheTTL is how long a fetched page is remembered, so a follow-up question
// about the same page (with another Focus) does not download it again. Zero
// disables the cache. Failures are never cached.
var CacheTTL = 10 * time.Minute

var safeHTTPClient = &http.Client{
	Timeout: 15 * time.Second,
	Transport: &http.Transport{
		DialContext:           safeDialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	},
}

// httpClient sends every request. Tests replace it to reach local servers,
// which the SSRF guard rightly refuses.
var httpClient = safeHTTPClient

var safeDialer = &net.Dialer{Timeout: 10 * time.Second}

// blockedNetworks are the special-purpose ranges the net.IP predicates
// below do not cover. Carrier-grade NAT space is the one that matters most:
// cloud providers put internal services there (Alibaba Cloud's metadata
// endpoint is 100.100.100.200), and none of IsPrivate, IsLoopback or
// IsLinkLocal recognises it. NAT64 is listed because 64:ff9b::a9fe:a9fe is
// 169.254.169.254 to a host that translates it.
var blockedNetworks = mustParseNetworks(
	"0.0.0.0/8",      // "this network"
	"100.64.0.0/10",  // carrier-grade NAT, cloud-internal services
	"192.0.0.0/24",   // IETF protocol assignments
	"198.18.0.0/15",  // benchmarking
	"240.0.0.0/4",    // reserved, and the broadcast address
	"64:ff9b::/96",   // NAT64 well-known prefix
	"64:ff9b:1::/48", // NAT64 local-use prefix
	"fec0::/10",      // deprecated site-local
	"2001::/32",      // Teredo, which embeds an IPv4 address
	"2002::/16",      // 6to4, which embeds an IPv4 address
)

func mustParseNetworks(cidrs ...string) []*net.IPNet {
	networks := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			panic(err)
		}
		networks = append(networks, network)
	}
	return networks
}

func isBlockedIP(ip net.IP) bool {
	if ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() ||
		ip.IsMulticast() {
		return true
	}
	for _, network := range blockedNetworks {
		if network.Contains(ip) {
			return true
		}
	}
	return false
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

// Options adjusts what FetchWithOptions returns.
type Options struct {
	// Focus is what the caller wants from the page, typically the user's
	// question ("harga emas 1 gram"). When the page is longer than
	// MaxChars, Content holds the passages that best match it instead of
	// the page's opening.
	Focus string

	// MaxChars caps Content, in characters. Zero means 4000.
	MaxChars int
}

// Result is the outcome of fetching one URL. If Error is non-empty, the
// fetch failed and the other fields are left empty.
type Result struct {
	URL     string `json:"url"`
	Title   string `json:"title,omitempty"`
	Content string `json:"content,omitempty"`
	// Published is the page's publication time when it states one.
	Published time.Time `json:"published,omitzero"`
	// Source says how Content was obtained; see the Source constants.
	Source string `json:"source,omitempty"`
	Error  string `json:"error,omitempty"`
}

// document is a fetched page, before Options shape it into a Result. It is
// cached and shared, so nothing may modify it after it is built.
type document struct {
	title     string
	published time.Time
	source    string
	facts     []string
	blocks    []block
}

// FetchMany fetches multiple URLs concurrently (one goroutine per URL) and
// returns one Result per URL, in the same order as urls. Requests are capped
// at 5 URLs per call; any beyond that are dropped. ctx controls cancellation
// and timeout for all fetches.
func FetchMany(ctx context.Context, urls []string) []Result {
	return FetchManyWithOptions(ctx, urls, Options{})
}

// FetchManyWithOptions is FetchMany with Options applied to every URL.
func FetchManyWithOptions(ctx context.Context, urls []string, opts Options) []Result {
	if len(urls) > maxURLsPerCall {
		urls = urls[:maxURLsPerCall]
	}

	results := make([]Result, len(urls))

	var wg sync.WaitGroup
	for i, pageURL := range urls {
		wg.Go(func() {
			result, err := FetchWithOptions(ctx, pageURL, opts)
			if err != nil {
				results[i] = Result{URL: pageURL, Error: err.Error()}
				return
			}
			results[i] = result
		})
	}
	wg.Wait()

	return results
}

// Fetch downloads pageURL and returns its title and the opening 4000
// characters of its readable text. It is FetchWithOptions without options.
//
// Both http and https URLs are fetched on purpose: many Indonesian sources
// are still http-only, and fetched content is transient (model context
// only) -- it is never persisted or executed. The SSRF guard applies to both
// schemes equally.
func Fetch(ctx context.Context, pageURL string) (title, content string, err error) {
	result, err := FetchWithOptions(ctx, pageURL, Options{})
	return result.Title, result.Content, err
}

// FetchWithOptions downloads pageURL and extracts its readable text: the
// main article when the page has one (go-readability), otherwise the whole
// page without its navigation, plus any tables the article left out and the
// facts the page states as structured data.
//
// When a site refuses the browser-like request (HTTP 202, 403, 429, or a
// Cloudflare challenge), the request is repeated once under
// HonestUserAgent; a second refusal returns ErrBlocked. Bodies larger than
// 5 MB (15 MB for a PDF) are rejected instead of being read into memory.
func FetchWithOptions(ctx context.Context, pageURL string, opts Options) (Result, error) {
	result := Result{URL: pageURL}

	u, err := url.Parse(pageURL)
	if err != nil {
		return result, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return result, fmt.Errorf("skema URL tidak didukung: %s", u.Scheme)
	}
	if u.Host == "" {
		return result, fmt.Errorf("URL tanpa host: %s", pageURL)
	}

	doc, err := cachedDocument(ctx, u)
	if err != nil {
		return result, err
	}

	result.Title = doc.title
	result.Published = doc.published
	result.Source = doc.source
	result.Content = present(doc, opts)

	return result, nil
}

func loadDocument(ctx context.Context, u *url.URL) (*document, error) {
	if locale, id, ok := msnArticle(u); ok {
		if doc, err := loadMSN(ctx, locale, id); err == nil {
			return doc, nil
		}
	}

	if host, title, ok := wikipediaArticle(u); ok {
		if doc, err := loadWikipedia(ctx, host, title); err == nil {
			return doc, nil
		}
	}

	return loadPage(ctx, u)
}

func loadPage(ctx context.Context, u *url.URL) (*document, error) {
	resp, err := get(ctx, u.String(), false, "")
	if err != nil {
		return nil, err
	}

	if refused(resp) {
		resp.Body.Close()

		resp, err = get(ctx, u.String(), true, "")
		if err != nil {
			return nil, err
		}
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNonAuthoritativeInfo:
	case refused(resp):
		return nil, fmt.Errorf("%w (status %d)", ErrBlocked, resp.StatusCode)
	default:
		return nil, fmt.Errorf("status %s", resp.Status)
	}

	contentType := resp.Header.Get("Content-Type")
	mediaType, _, _ := mime.ParseMediaType(contentType)
	mediaType = strings.ToLower(mediaType)

	kind := contentKind(mediaType, u)
	if kind == kindUnsupported {
		// Decided from the header alone, before a video or an archive is
		// downloaded only to be thrown away.
		return nil, fmt.Errorf("konten bukan HTML (%s)", contentType)
	}

	limit := int64(maxBodyBytes)
	if kind == kindPDF || kind == kindBinary {
		limit = maxPDFBytes
	}

	if kind == kindBinary {
		// A generic binary type is read only when it is a PDF in disguise,
		// as download links often serve them.
		head := make([]byte, 5)
		n, _ := io.ReadFull(resp.Body, head)
		if !bytes.Equal(head[:n], []byte("%PDF-")) {
			return nil, fmt.Errorf("konten bukan HTML (%s)", contentType)
		}
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(head[:n]), resp.Body), resp.Body}
		kind = kindPDF
	}

	body, err := readBody(resp, limit)
	if err != nil {
		return nil, err
	}

	switch {
	case kind == kindPDF || bytes.HasPrefix(body, []byte("%PDF-")):
		return documentFromPDF(ctx, body, u)
	case kind == kindText:
		return documentFromText(decodeCharset(body, contentType), u)
	default:
		return documentFromHTML(decodeCharset(body, contentType), u)
	}
}

type contentKindValue int

const (
	kindHTML contentKindValue = iota
	kindText
	kindPDF
	kindBinary // octet-stream: read only if it turns out to be a PDF
	kindUnsupported
)

// contentKind decides from the Content-Type header what the body is. A
// missing type is read as HTML, as browsers do.
func contentKind(mediaType string, u *url.URL) contentKindValue {
	switch {
	case mediaType == "" || strings.Contains(mediaType, "html"):
		return kindHTML
	case mediaType == "text/plain":
		return kindText
	case isPDF(mediaType, u):
		return kindPDF
	case mediaType == "application/octet-stream" || mediaType == "binary/octet-stream" ||
		mediaType == "application/x-download" || mediaType == "application/force-download":
		return kindBinary
	default:
		return kindUnsupported
	}
}

// refused reports whether the site turned the request away as automated:
// a challenge page (Kompas answers 202), a refusal, a rate limit, or a
// Cloudflare challenge.
func refused(resp *http.Response) bool {
	switch resp.StatusCode {
	case http.StatusAccepted, http.StatusForbidden, http.StatusTooManyRequests:
		return true
	case http.StatusServiceUnavailable:
		return resp.Header.Get("Cf-Mitigated") != ""
	}
	return false
}

func get(ctx context.Context, rawURL string, honest bool, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}

	if accept == "" {
		accept = "text/html,application/xhtml+xml,application/xml;q=0.9,application/pdf;q=0.8,*/*;q=0.7"
	}

	if honest {
		req.Header.Set("User-Agent", HonestUserAgent)
		req.Header.Set("Api-User-Agent", HonestUserAgent)
	} else {
		req.Header.Set("User-Agent", browserUserAgent)
		req.Header.Set("Sec-Fetch-Dest", "document")
		req.Header.Set("Sec-Fetch-Mode", "navigate")
		req.Header.Set("Sec-Fetch-Site", "none")
		req.Header.Set("Upgrade-Insecure-Requests", "1")
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("Accept-Language", "id-ID,id;q=0.9,en-US;q=0.8,en;q=0.7")

	return doWithRetry(httpClient, req)
}

func readBody(resp *http.Response, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%w (> %d bytes)", errTooLarge, limit)
	}
	return body, nil
}

// decodeCharset converts body to UTF-8, reading the charset from the
// Content-Type header or the page's own meta tag. Older Indonesian sites
// still serve windows-1252 and ISO-8859-1.
//
// A body that is already valid UTF-8 is kept as it is, whatever it claims:
// without a declaration in its first kilobyte, DetermineEncoding guesses
// windows-1252 and would turn "café" into "cafÃ©", and a page that declares
// a legacy charset but is valid UTF-8 is mislabelled far more often than
// not. Legacy text with accented letters is almost never valid UTF-8.
func decodeCharset(body []byte, contentType string) []byte {
	if utf8.Valid(body) {
		return body
	}

	enc, name, _ := charset.DetermineEncoding(body, contentType)
	if name == "utf-8" || enc == nil {
		return body
	}

	decoded, err := enc.NewDecoder().Bytes(body)
	if err != nil {
		return body
	}
	return decoded
}

func documentFromHTML(body []byte, u *url.URL) (*document, error) {
	root, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	meta := readMeta(root)
	data := readStructured(root)

	doc := &document{
		title:     firstNonEmpty(meta.title, data.title),
		published: firstTime(data.published, meta.published),
		facts:     data.facts,
	}

	var articleBlocks []block
	articleTitle := ""

	// FromDocument works on a copy, so root stays whole for the page text
	// below.
	if article, err := readability.FromDocument(root, u); err == nil && article.Node != nil {
		articleBlocks = renderBlocks(article.Node)
		articleTitle = cleanText(article.Title())
		if doc.published.IsZero() {
			if published, err := article.PublishedTime(); err == nil {
				doc.published = parseDate(published.Format(time.RFC3339))
			}
		}
	}

	pageBlocks := renderBlocks(pageBody(root))

	articleLen, pageLen := blocksLen(articleBlocks), blocksLen(pageBlocks)

	switch {
	// An "article" that is a sliver of a much longer page is readability
	// guessing on a listing or a data page; the page itself reads better.
	case articleLen >= minReadableChars && !(articleLen < 500 && pageLen > 4*articleLen):
		doc.blocks = append(append(articleBlocks, missingTables(articleBlocks, pageBlocks)...), data.blocks...)
		doc.source = SourceArticle
		doc.title = firstNonEmpty(articleTitle, doc.title)
	case pageLen >= minPageChars:
		doc.blocks = append(pageBlocks, data.blocks...)
		doc.source = SourcePage
	case meta.description != "" || len(data.blocks) > 0 || len(doc.facts) > 0:
		// Too little text to be the page, but the page describes itself:
		// what is there is most likely a JavaScript application's shell
		// ("Loading...", "Enable JavaScript"), and its description says
		// more than the shell does.
		doc.blocks = data.blocks
		if meta.description != "" {
			doc.blocks = append([]block{{kind: blockText, text: meta.description}}, doc.blocks...)
		}
		doc.source = SourceSummary
	case pageLen > 0:
		// A page that is genuinely this short, with nothing else to say.
		doc.blocks = pageBlocks
		doc.source = SourcePage
	default:
		return nil, ErrNoContent
	}

	return doc, nil
}

// missingTables returns the data tables of the whole page that the article
// text left out. Readability drops tables now and then, and tables hold
// exactly what people ask a page about: prices, rates, schedules.
func missingTables(article, page []block) []block {
	have := map[string]bool{}
	lastTable := 0
	for _, b := range article {
		if b.kind == blockRow {
			have[b.text] = true
			lastTable = max(lastTable, b.table)
		}
	}

	var order []int
	rows := map[int][]block{}
	for _, b := range page {
		if b.kind != blockRow {
			continue
		}
		if _, seen := rows[b.table]; !seen {
			order = append(order, b.table)
		}
		rows[b.table] = append(rows[b.table], b)
	}

	var out []block
	for _, id := range order {
		table := rows[id]
		if len(table) < 2 || have[table[0].text] || have[table[len(table)-1].text] {
			continue
		}

		lastTable++
		for _, row := range table {
			row.table = lastTable
			out = append(out, row)
		}
		if len(out) > 400 {
			break
		}
	}

	if len(out) == 0 {
		return nil
	}
	return append([]block{{kind: blockHeading, text: "Tabel di halaman ini"}}, out...)
}

func documentFromText(body []byte, u *url.URL) (*document, error) {
	if !utf8.Valid(body) {
		return nil, fmt.Errorf("konten bukan teks yang bisa dibaca")
	}

	blocks := textBlocks(string(body))
	if len(blocks) == 0 {
		return nil, ErrNoContent
	}

	return &document{title: firstLineTitle(blocks), source: SourceText, blocks: blocks}, nil
}

// extractReadable is the article path of FetchWithOptions on its own:
// readability's article rendered as text, or ok=false when the page has
// none long enough to read.
func extractReadable(body []byte, pageURL *url.URL) (title, content string, ok bool) {
	article, err := readability.FromReader(bytes.NewReader(body), pageURL)
	if err != nil || article.Node == nil {
		return "", "", false
	}

	blocks := renderBlocks(article.Node)
	if blocksLen(blocks) < minReadableChars {
		return "", "", false
	}

	return article.Title(), blocksText(blocks), true
}

// extractCleanText is the page path of FetchWithOptions on its own: the
// page's visible text without its navigation chrome.
func extractCleanText(doc *goquery.Document) string {
	if len(doc.Nodes) == 0 {
		return ""
	}
	return blocksText(renderBlocks(pageBody(doc.Nodes[0])))
}

type cachedDoc struct {
	doc     *document
	expires time.Time
}

var docCache = struct {
	sync.Mutex
	entries map[string]cachedDoc
}{entries: map[string]cachedDoc{}}

func cachedDocument(ctx context.Context, u *url.URL) (*document, error) {
	key := *u
	key.Fragment, key.RawFragment = "", ""
	cacheKey := key.String()

	if CacheTTL > 0 {
		docCache.Lock()
		entry, ok := docCache.entries[cacheKey]
		docCache.Unlock()
		if ok && time.Now().Before(entry.expires) {
			return entry.doc, nil
		}
	}

	doc, err := loadDocument(ctx, u)
	if err != nil {
		return nil, err
	}

	if CacheTTL > 0 {
		docCache.Lock()
		defer docCache.Unlock()

		now := time.Now()
		if len(docCache.entries) >= cacheCapacity {
			for k, entry := range docCache.entries {
				if now.After(entry.expires) {
					delete(docCache.entries, k)
				}
			}
		}
		if len(docCache.entries) >= cacheCapacity {
			oldest := ""
			for k, entry := range docCache.entries {
				if oldest == "" || entry.expires.Before(docCache.entries[oldest].expires) {
					oldest = k
				}
			}
			delete(docCache.entries, oldest)
		}
		docCache.entries[cacheKey] = cachedDoc{doc: doc, expires: now.Add(CacheTTL)}
	}

	return doc, nil
}

func resetCache() {
	docCache.Lock()
	defer docCache.Unlock()
	docCache.entries = map[string]cachedDoc{}
}

func doWithRetry(client *http.Client, req *http.Request) (*http.Response, error) {
	resp, err := client.Do(req)
	if err == nil || req.Context().Err() != nil {
		return resp, err
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return nil, err
	}

	select {
	case <-time.After(500 * time.Millisecond):
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
	return client.Do(req)
}
