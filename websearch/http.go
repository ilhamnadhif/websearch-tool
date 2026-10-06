package websearch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// maxPageBytes caps one engine response. A results page is a few hundred
// kilobytes; anything far larger is not a results page.
const maxPageBytes = 4 << 20

// minAttemptBudget is the least time worth starting an engine request with.
// When the caller's deadline leaves less, the engine is skipped instead of
// started and cut off half way.
const minAttemptBudget = 1500 * time.Millisecond

var browserUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36"

// HonestUserAgent identifies the client to the APIs that ask callers to say
// who they are (Wikipedia, Brave). Set it to your application's name and a
// contact address, e.g. "MyBot/1.0 (+https://example.com; ops@example.com)".
var HonestUserAgent = "websearch-tool/1.3 (+https://github.com/ilhamnadhif/websearch-tool)"

// CookieDir is where the per-engine cookie files live. Empty means
// <user cache dir>/websearch-tool. Cookies cut down on challenges from the
// scraped engines; when the directory cannot be created, searches simply run
// without them.
var CookieDir = ""

var transport = &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	ForceAttemptHTTP2:     true,
	MaxIdleConns:          32,
	MaxIdleConnsPerHost:   4,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   5 * time.Second,
	ExpectContinueTimeout: time.Second,
}

// page is one HTTP answer, read in full.
type page struct {
	status int
	header http.Header
	body   []byte
	url    *url.URL // final URL after redirects
}

type requestOptions struct {
	browser    bool        // send a desktop browser's headers instead of HonestUserAgent
	cookieFile string      // persist cookies for this engine; empty means none
	header     http.Header // extra headers, applied last
}

// fetch performs one GET and reads the whole body. Every status is returned
// to the caller, which knows what each one means for its engine. A transport
// error is retried once unless it was a timeout or the context ended.
func fetch(ctx context.Context, rawURL string, opts requestOptions) (*page, error) {
	target, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}

	if opts.browser {
		setBrowserHeaders(req.Header)
	} else {
		req.Header.Set("User-Agent", HonestUserAgent)
		req.Header.Set("Accept-Language", "id-ID,id;q=0.9,en;q=0.7")
	}
	for name, values := range opts.header {
		req.Header[name] = values
	}

	client := &http.Client{Transport: transport}

	var cookiePath string
	if opts.cookieFile != "" {
		if path, err := cookieFilePath(opts.cookieFile); err == nil {
			if jar, err := cookiejar.New(nil); err == nil {
				cookiePath = path
				loadCookies(cookiePath, target, jar)
				client.Jar = jar
			}
		}
	}

	resp, err := doWithRetry(client, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPageBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxPageBytes {
		return nil, fmt.Errorf("respons terlalu besar (> %d bytes)", maxPageBytes)
	}

	if client.Jar != nil {
		saveCookies(cookiePath, target, client.Jar)
	}

	return &page{status: resp.StatusCode, header: resp.Header, body: body, url: resp.Request.URL}, nil
}

func setBrowserHeaders(h http.Header) {
	h.Set("User-Agent", browserUserAgent)
	h.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	h.Set("Accept-Language", "id-ID,id;q=0.9,en-US;q=0.8,en;q=0.7")
	h.Set("Sec-Ch-Ua", `"Google Chrome";v="141", "Not?A_Brand";v="8", "Chromium";v="141"`)
	h.Set("Sec-Ch-Ua-Mobile", "?0")
	h.Set("Sec-Ch-Ua-Platform", `"Windows"`)
	h.Set("Sec-Fetch-Dest", "document")
	h.Set("Sec-Fetch-Mode", "navigate")
	h.Set("Sec-Fetch-Site", "none")
	h.Set("Sec-Fetch-User", "?1")
	h.Set("Upgrade-Insecure-Requests", "1")
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

// cookieMu guards the cookie files only. It is never held across a request,
// so one slow engine cannot hold up searches on the others.
var cookieMu sync.Mutex

func cookieFilePath(name string) (string, error) {
	dir := CookieDir
	if dir == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(cache, "websearch-tool")
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}

	return filepath.Join(dir, name), nil
}

func loadCookies(path string, target *url.URL, jar http.CookieJar) {
	cookieMu.Lock()
	data, err := os.ReadFile(path)
	cookieMu.Unlock()
	if err != nil {
		return
	}

	var cookies []*http.Cookie
	if err := json.Unmarshal(data, &cookies); err != nil {
		return
	}

	jar.SetCookies(target, cookies)
}

// saveCookies writes the jar's cookies for target through a temporary file
// and a rename, so a concurrent reader never sees half a file. A jar with no
// cookies leaves the file alone rather than wiping what an earlier search
// stored.
func saveCookies(path string, target *url.URL, jar http.CookieJar) {
	cookies := jar.Cookies(target)
	if len(cookies) == 0 {
		return
	}

	data, err := json.Marshal(cookies)
	if err != nil {
		return
	}

	cookieMu.Lock()
	defer cookieMu.Unlock()

	tmp, err := os.CreateTemp(filepath.Dir(path), ".cookies-*")
	if err != nil {
		return
	}
	defer os.Remove(tmp.Name())

	// CreateTemp opens the file 0600: it holds session cookies, and other
	// users must not read it.
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return
	}
	if err := tmp.Close(); err != nil {
		return
	}

	_ = os.Rename(tmp.Name(), path)
}

// gate paces the requests sent to one engine and holds it back after the
// engine refused us. Each engine has its own, so a slow or blocked engine
// never delays the others.
type gate struct {
	interval time.Duration

	mu     sync.Mutex
	next   time.Time // earliest start for the next request
	until  time.Time // end of the current cool-down
	reason error     // why the engine is cooling down
}

var errNoTimeLeft = errors.New("waktu tidak cukup untuk engine ini")

// coolingDownError is the refusal of an engine that is cooling down. It
// carries the reason, so a key the API rejected an hour ago is still
// reported as a rejected key rather than as a block.
type coolingDownError struct{ reason error }

func (e *coolingDownError) Error() string {
	return "sedang didiamkan: " + e.reason.Error()
}

func (e *coolingDownError) Unwrap() error { return e.reason }

// wait reserves the next request slot and sleeps until it opens. It refuses
// at once, without sleeping, while the engine is cooling down or when the
// slot would open too close to ctx's deadline to be useful.
func (g *gate) wait(ctx context.Context) error {
	g.mu.Lock()

	now := time.Now()
	if now.Before(g.until) {
		reason := g.reason
		g.mu.Unlock()
		return &coolingDownError{reason: reason}
	}

	start := g.next
	if start.Before(now) {
		start = now
	}

	if deadline, ok := ctx.Deadline(); ok && deadline.Sub(start) < minAttemptBudget {
		g.mu.Unlock()
		return errNoTimeLeft
	}

	g.next = start.Add(g.interval)
	g.mu.Unlock()

	delay := start.Sub(now)
	if delay <= 0 {
		return nil
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// coolDown keeps the engine out of rotation for d, extending (never
// shortening) a cool-down already running, and remembers why.
func (g *gate) coolDown(d time.Duration, reason error) {
	if d <= 0 {
		return
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if until := time.Now().Add(d); until.After(g.until) {
		g.until = until
		g.reason = reason
	}
}

func (g *gate) reset() {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.next = time.Time{}
	g.until = time.Time{}
	g.reason = nil
}
