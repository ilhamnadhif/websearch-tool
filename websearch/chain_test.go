package websearch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// stubEngines points every engine at one local server whose handlers the
// test chooses, zeroes the pacing so tests run fast, and restores the
// package settings afterwards. Engines without a handler answer 500.
type stubEngines struct {
	srv  *httptest.Server
	hits map[string]*atomic.Int32
}

func newStubEngines(t *testing.T, handlers map[string]http.HandlerFunc) *stubEngines {
	t.Helper()

	saved := struct {
		bing, ddg, lite, news, wiki, brave, key string
		engines                                 []string
		ttl, attempt                            time.Duration
		cookieDir                               string
		intervals                               map[string]time.Duration
	}{
		SearchEndpoint, DuckDuckGoEndpoint, DuckDuckGoLiteEndpoint, BingNewsEndpoint, WikipediaEndpoint, BraveEndpoint, BraveAPIKey,
		Engines, CacheTTL, AttemptTimeout, CookieDir, map[string]time.Duration{},
	}
	for name, e := range engines {
		saved.intervals[name] = e.gate.interval
		e.gate.interval = 0
	}

	stub := &stubEngines{hits: map[string]*atomic.Int32{}}
	mux := http.NewServeMux()
	for _, path := range []string{"/bing", "/ddg/html/", "/ddg/lite/", "/news", "/w/api.php", "/brave"} {
		counter := &atomic.Int32{}
		stub.hits[path] = counter
		handler := handlers[path]
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			counter.Add(1)
			if handler == nil {
				http.Error(w, "no stub", http.StatusInternalServerError)
				return
			}
			handler(w, r)
		})
	}
	stub.srv = httptest.NewServer(mux)

	SearchEndpoint = stub.srv.URL + "/bing"
	DuckDuckGoEndpoint = stub.srv.URL + "/ddg/html"
	DuckDuckGoLiteEndpoint = stub.srv.URL + "/ddg/lite/"
	BingNewsEndpoint = stub.srv.URL + "/news"
	WikipediaEndpoint = stub.srv.URL + "/w/api.php"
	BraveEndpoint = stub.srv.URL + "/brave"
	BraveAPIKey = ""
	Engines = []string{EngineBrave, EngineBing, EngineDuckDuckGo, EngineBingNews, EngineWikipedia}
	CacheTTL = 0
	AttemptTimeout = 3 * time.Second
	CookieDir = t.TempDir()
	resetState()

	t.Cleanup(func() {
		stub.srv.Close()
		SearchEndpoint, DuckDuckGoEndpoint, DuckDuckGoLiteEndpoint = saved.bing, saved.ddg, saved.lite
		BingNewsEndpoint, WikipediaEndpoint, BraveEndpoint, BraveAPIKey = saved.news, saved.wiki, saved.brave, saved.key
		Engines, CacheTTL, AttemptTimeout, CookieDir = saved.engines, saved.ttl, saved.attempt, saved.cookieDir
		for name, e := range engines {
			e.gate.interval = saved.intervals[name]
		}
		resetState()
	})

	return stub
}

func (s *stubEngines) count(path string) int { return int(s.hits[path].Load()) }

func html200(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, body)
	}
}

func status(code int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
		fmt.Fprint(w, body)
	}
}

const bingSoftBlock = `<html><body><ol id="b_results"><li class="b_no"><h1>Tidak ada hasil untuk <strong>q</strong></h1></li></ol></body></html>`

func bingPage(blocks ...string) string {
	return `<html><body><ol id="b_results">` + strings.Join(blocks, "") + `</ol></body></html>`
}

func ddgPage(blocks ...string) string {
	return `<html><body><div class="results">` + strings.Join(blocks, "") + `</div></body></html>`
}

const ddgChallenge = `<html><body><center id="lite_wrapper"><form id="challenge-form" action="//duckduckgo.com/anomaly.js?sv=lite&cc=botnet" method="POST"></form></center></body></html>`

var goldResults = []string{
	ddgResultBlock(ddgRedirect("https://www.logammulia.com/id/harga-emas-hari-ini"), "Harga Emas Antam Hari Ini", "Harga emas batangan Antam per gram hari ini."),
	ddgResultBlock(ddgRedirect("https://www.cnbcindonesia.com/market/emas"), "Harga Emas Antam Naik", "Harga emas Antam naik Rp 5.000 per gram."),
}

func outcomes(resp Response) string {
	parts := make([]string, len(resp.Attempts))
	for i, a := range resp.Attempts {
		parts[i] = a.Engine + "=" + a.Outcome
	}
	return strings.Join(parts, ",")
}

func TestSearchMovesPastBingsSoftBlock(t *testing.T) {
	stub := newStubEngines(t, map[string]http.HandlerFunc{
		"/bing":      html200(bingSoftBlock),
		"/ddg/html/": html200(ddgPage(goldResults...)),
	})

	resp, err := Search(context.Background(), "harga emas antam hari ini")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if resp.Engine != EngineDuckDuckGo || len(resp.Results) != 2 {
		t.Fatalf("engine=%q results=%d attempts=%s", resp.Engine, len(resp.Results), outcomes(resp))
	}
	if got := outcomes(resp); got != "bing=empty,duckduckgo=ok" {
		t.Errorf("attempts = %s", got)
	}
	if resp.Results[0].URL != "https://www.logammulia.com/id/harga-emas-hari-ini" {
		t.Errorf("first URL = %q", resp.Results[0].URL)
	}
	if stub.count("/news") != 0 || stub.count("/w/api.php") != 0 {
		t.Error("engines after the one that answered were still asked")
	}
}

func TestSearchDiscardsDecoyResults(t *testing.T) {
	newStubEngines(t, map[string]http.HandlerFunc{
		"/bing": html200(bingPage(
			algoBlock(bingRedirect("https://drive.google.com/file/d/abc"), "Google Drive: Sign-in", "Access Google Drive with a Google account."),
			algoBlock(bingRedirect("https://account.microsoft.com/code"), "Your Microsoft verification code", "Use this code for Microsoft account verification."),
			algoBlock(bingRedirect("https://example-adult.test/video"), "Hot videos tonight", "Watch now."),
		)),
		"/ddg/html/": html200(ddgPage(goldResults...)),
	})

	resp, err := Search(context.Background(), "harga emas antam hari ini")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got := outcomes(resp); got != "bing=unrelated,duckduckgo=ok" {
		t.Fatalf("attempts = %s", got)
	}
	for _, r := range resp.Results {
		if strings.Contains(r.URL, "google.com") || strings.Contains(r.URL, "microsoft.com") {
			t.Errorf("decoy result returned: %+v", r)
		}
	}
}

func TestSearchDropsStrayResultsAndRenumbers(t *testing.T) {
	newStubEngines(t, map[string]http.HandlerFunc{
		"/bing": html200(bingPage(
			algoBlock(bingRedirect("https://www.krl.co.id/jadwal"), "Jadwal KRL Commuter Line Bogor", "Jadwal KRL Bogor - Jakarta Kota."),
			algoBlock(bingRedirect("https://stray.test/x"), "Promo kartu kredit", "Diskon besar."),
			algoBlock(bingRedirect("https://www.kompas.com/krl"), "KRL Bogor Jakarta Kota hari ini", "Perjalanan KRL."),
		)),
	})

	resp, err := Search(context.Background(), "jadwal krl bogor jakarta kota")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("results = %+v, want the stray one dropped", resp.Results)
	}
	if resp.Results[1].Rank != 2 || !strings.Contains(resp.Results[1].URL, "kompas") {
		t.Errorf("second result = %+v, want kompas renumbered to rank 2", resp.Results[1])
	}
}

func TestSearchCoolsDownAfterAChallenge(t *testing.T) {
	stub := newStubEngines(t, map[string]http.HandlerFunc{
		"/bing":      html200(bingSoftBlock),
		"/ddg/html/": status(http.StatusAccepted, ddgChallenge),
		"/ddg/lite/": html200(`<html><body>never asked</body></html>`),
		"/news":      status(http.StatusOK, `<?xml version="1.0"?><rss version="2.0"><channel></channel></rss>`),
		"/w/api.php": status(http.StatusOK, `{"query":{"search":[]}}`),
	})

	resp, err := Search(context.Background(), "harga emas antam")
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("err = %v, want ErrBlocked when one engine refused and the rest were empty", err)
	}
	if got := outcomes(resp); got != "bing=empty,duckduckgo=blocked,bing-news=empty,wikipedia=empty" {
		t.Errorf("first attempts = %s", got)
	}
	if stub.count("/ddg/lite/") != 0 {
		t.Error("the lite endpoint was asked after a challenge; both share one anti-bot check")
	}

	resp, _ = Search(context.Background(), "harga emas antam")
	if got := outcomes(resp); !strings.Contains(got, "duckduckgo=cooldown") {
		t.Errorf("second attempts = %s, want duckduckgo skipped while cooling down", got)
	}
	if stub.count("/ddg/html/") != 1 {
		t.Errorf("duckduckgo asked %d times, want 1", stub.count("/ddg/html/"))
	}
}

func TestSearchFallsBackToDuckDuckGoLiteWhenMarkupChanges(t *testing.T) {
	lite := `<html><body><table>
		<tr><td>1.</td><td><a rel="nofollow" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fwww.pajak.go.id%2Fid%2Fefiling" class="result-link">Cara lapor SPT tahunan e-Filing</a></td></tr>
		<tr><td></td><td class="result-snippet">Panduan lapor SPT tahunan lewat e-Filing DJP.</td></tr>
		<tr><td></td><td><span class="link-text">www.pajak.go.id</span></td></tr>
		<tr class="result-sponsored"><td>2.</td><td><a rel="nofollow" href="https://ads.test/spt" class="result-link">Iklan SPT</a></td></tr>
		<tr class="result-sponsored"><td></td><td class="result-snippet">Iklan.</td></tr>
	</table></body></html>`

	newStubEngines(t, map[string]http.HandlerFunc{
		"/bing":      html200(bingSoftBlock),
		"/ddg/html/": html200(`<html><body><div class="new-layout">tanpa hasil</div></body></html>`),
		"/ddg/lite/": html200(lite),
	})

	resp, err := Search(context.Background(), "cara lapor spt tahunan")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(resp.Results) != 1 {
		t.Fatalf("results = %+v, want the one organic lite result", resp.Results)
	}
	r := resp.Results[0]
	if r.URL != "https://www.pajak.go.id/id/efiling" || r.Snippet != "Panduan lapor SPT tahunan lewat e-Filing DJP." || r.Site != "pajak.go.id" {
		t.Errorf("lite result = %+v", r)
	}
}

func TestSearchEverythingEmptyIsAnHonestEmptyAnswer(t *testing.T) {
	newStubEngines(t, map[string]http.HandlerFunc{
		"/bing":      html200(bingSoftBlock),
		"/ddg/html/": html200(`<html><body><div class="no-results">No results.</div></body></html>`),
		"/news":      status(http.StatusOK, `<?xml version="1.0"?><rss version="2.0"><channel></channel></rss>`),
		"/w/api.php": status(http.StatusOK, `{"query":{"search":[]}}`),
	})

	resp, err := Search(context.Background(), "qwxzzvbnm lkjqwpo")
	if err != nil {
		t.Fatalf("err = %v, want nil for an answer every engine agrees is empty", err)
	}
	if len(resp.Results) != 0 || resp.Engine != "" {
		t.Errorf("resp = %+v", resp)
	}
	if len(resp.Attempts) != 4 {
		t.Errorf("attempts = %s, want all four engines asked", outcomes(resp))
	}
}

func TestSearchReportsBrokenMarkupOnlyWhenNothingElseAnswered(t *testing.T) {
	newStubEngines(t, map[string]http.HandlerFunc{
		"/bing":      html200(`<html><body><div>tata letak baru</div></body></html>`),
		"/ddg/html/": html200(`<html><body><div>tata letak baru</div></body></html>`),
		"/ddg/lite/": html200(`<html><body><div>tata letak baru</div></body></html>`),
		"/news":      status(http.StatusOK, `<?xml version="1.0"?><rss version="2.0"><channel></channel></rss>`),
		"/w/api.php": status(http.StatusOK, `{"query":{"search":[]}}`),
	})

	_, err := Search(context.Background(), "jadwal sholat bandung")
	if !errors.Is(err, ErrResultsNotParsed) {
		t.Fatalf("err = %v, want ErrResultsNotParsed", err)
	}
}

func TestSearchUsesBraveFirstWhenKeyed(t *testing.T) {
	var token, country, lang atomic.Value
	stub := newStubEngines(t, map[string]http.HandlerFunc{
		"/brave": func(w http.ResponseWriter, r *http.Request) {
			token.Store(r.Header.Get("X-Subscription-Token"))
			country.Store(r.URL.Query().Get("country"))
			lang.Store(r.URL.Query().Get("search_lang"))
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"web":{"results":[
				{"title":"Harga <strong>iPhone 17</strong> di Indonesia","url":"https://www.ibox.co.id/iphone-17","description":"Harga resmi <strong>iPhone 17</strong> mulai Rp16 juta.","page_age":"2026-10-03T06:07:00"},
				{"title":"Spesifikasi iPhone 17","url":"https://www.apple.com/id/iphone-17/","description":"iPhone 17 Indonesia."}
			]}}`)
		},
	})
	BraveAPIKey = "test-key"

	resp, err := Search(context.Background(), "harga iPhone 17 Indonesia")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if resp.Engine != EngineBrave || stub.count("/bing") != 0 {
		t.Fatalf("engine = %q, bing hits = %d", resp.Engine, stub.count("/bing"))
	}
	if token.Load() != "test-key" || country.Load() != "ID" || lang.Load() != "id" {
		t.Errorf("brave request: token=%v country=%v lang=%v", token.Load(), country.Load(), lang.Load())
	}
	first := resp.Results[0]
	if first.Title != "Harga iPhone 17 di Indonesia" || strings.Contains(first.Snippet, "<") {
		t.Errorf("markup left in result: %+v", first)
	}
	if want := time.Date(2026, 10, 3, 6, 7, 0, 0, time.UTC); !first.Published.Equal(want) {
		t.Errorf("published = %v, want %v", first.Published, want)
	}
}

func TestSearchBraveRateLimitCoolsDownForRetryAfter(t *testing.T) {
	newStubEngines(t, map[string]http.HandlerFunc{
		"/brave": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
		},
		"/bing": html200(bingPage(algoBlock(bingRedirect("https://www.bmkg.go.id/cuaca/jakarta"), "Prakiraan Cuaca Jakarta", "Cuaca Jakarta besok."))),
	})
	BraveAPIKey = "test-key"

	resp, err := Search(context.Background(), "cuaca jakarta besok")
	if err != nil || resp.Engine != EngineBing {
		t.Fatalf("engine=%q err=%v attempts=%s", resp.Engine, err, outcomes(resp))
	}

	brave := engines[EngineBrave].gate
	brave.mu.Lock()
	left := time.Until(brave.until)
	brave.mu.Unlock()
	if left < 100*time.Second || left > 121*time.Second {
		t.Errorf("brave cool-down = %v, want about the 120s Retry-After", left)
	}
}

func TestSearchReadsBingNewsRSS(t *testing.T) {
	feed := `<?xml version="1.0" encoding="utf-8" ?><rss version="2.0" xmlns:News="https://www.bing.com/news/search?format=rss"><channel><title>x</title>` +
		`<item><title>Tarif Listrik Oktober 2026 Resmi Berlaku</title>` +
		`<link>http://www.bing.com/news/apiclick.aspx?ref=FexRss&amp;aid=&amp;tid=1&amp;url=https%3a%2f%2fmoney.kompas.com%2fread%2f2026%2f10%2f03%2ftarif-listrik&amp;c=1&amp;mkt=en-id</link>` +
		`<description>Simak tarif listrik per kWh Oktober 2026.</description><pubDate>Sat, 03 Oct 2026 06:07:00 GMT</pubDate><News:Source>Kompas Money</News:Source></item>` +
		`</channel></rss>`

	newStubEngines(t, map[string]http.HandlerFunc{
		"/bing":      html200(bingSoftBlock),
		"/ddg/html/": status(http.StatusAccepted, ddgChallenge),
		"/news": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("format") != "rss" || r.URL.Query().Get("mkt") != "id-ID" {
				http.Error(w, "bad query", http.StatusBadRequest)
				return
			}
			fmt.Fprint(w, feed)
		},
	})

	resp, err := Search(context.Background(), "tarif listrik per kwh oktober 2026")
	if err != nil {
		t.Fatalf("Search: %v (attempts %s)", err, outcomes(resp))
	}
	if resp.Engine != EngineBingNews || len(resp.Results) != 1 {
		t.Fatalf("engine=%q results=%+v", resp.Engine, resp.Results)
	}
	r := resp.Results[0]
	if r.URL != "https://money.kompas.com/read/2026/10/03/tarif-listrik" || r.Site != "money.kompas.com" {
		t.Errorf("news link not unwrapped: %+v", r)
	}
	if want := time.Date(2026, 10, 3, 6, 7, 0, 0, time.UTC); !r.Published.Equal(want) {
		t.Errorf("published = %v, want %v", r.Published, want)
	}
}

func TestSearchAsksWikipediaHonestly(t *testing.T) {
	var ua atomic.Value
	newStubEngines(t, map[string]http.HandlerFunc{
		"/bing":      html200(bingSoftBlock),
		"/ddg/html/": status(http.StatusAccepted, ddgChallenge),
		"/news":      status(http.StatusOK, `<?xml version="1.0"?><rss version="2.0"><channel></channel></rss>`),
		"/w/api.php": func(w http.ResponseWriter, r *http.Request) {
			ua.Store(r.Header.Get("User-Agent"))
			fmt.Fprint(w, `{"query":{"search":[{"title":"Menteri Keuangan Indonesia","snippet":"<span class=\"searchmatch\">Menteri</span> <span class=\"searchmatch\">Keuangan</span> &amp; Kementerian"}]}}`)
		},
	})

	resp, err := Search(context.Background(), "siapa menteri keuangan indonesia sekarang")
	if err != nil {
		t.Fatalf("Search: %v (attempts %s)", err, outcomes(resp))
	}
	if ua.Load() != HonestUserAgent {
		t.Errorf("wikipedia user agent = %v, want HonestUserAgent", ua.Load())
	}
	r := resp.Results[0]
	if !strings.HasSuffix(r.URL, "/wiki/Menteri_Keuangan_Indonesia") || r.Snippet != "Menteri Keuangan & Kementerian" {
		t.Errorf("wikipedia result = %+v", r)
	}
}

func TestSearchCachesOnlyAnswers(t *testing.T) {
	stub := newStubEngines(t, map[string]http.HandlerFunc{
		"/bing": html200(bingPage(algoBlock(bingRedirect("https://www.bmkg.go.id/cuaca"), "Cuaca Bandung", "Prakiraan cuaca Bandung."))),
	})
	CacheTTL = time.Minute

	if _, err := Search(context.Background(), "cuaca bandung"); err != nil {
		t.Fatal(err)
	}
	resp, err := Search(context.Background(), "  Cuaca   BANDUNG ")
	if err != nil || !resp.Cached || len(resp.Results) != 1 {
		t.Fatalf("second search: cached=%v results=%d err=%v", resp.Cached, len(resp.Results), err)
	}
	if stub.count("/bing") != 1 {
		t.Errorf("bing asked %d times, want 1", stub.count("/bing"))
	}

	resp.Results[0].Title = "dirusak pemanggil"
	again, _ := Search(context.Background(), "cuaca bandung")
	if again.Results[0].Title != "Cuaca Bandung" {
		t.Error("a caller's edit leaked into the cache")
	}
}

func TestSearchDoesNotCacheEmptyAnswers(t *testing.T) {
	stub := newStubEngines(t, map[string]http.HandlerFunc{
		"/bing":      html200(bingSoftBlock),
		"/ddg/html/": html200(`<html><body><div class="no-results">No results.</div></body></html>`),
		"/news":      status(http.StatusOK, `<?xml version="1.0"?><rss version="2.0"><channel></channel></rss>`),
		"/w/api.php": status(http.StatusOK, `{"query":{"search":[]}}`),
	})
	CacheTTL = time.Minute

	for range 2 {
		if _, err := Search(context.Background(), "tidak ada apa apa"); err != nil {
			t.Fatal(err)
		}
	}
	if stub.count("/bing") != 2 {
		t.Errorf("bing asked %d times, want 2: an empty answer must not be cached", stub.count("/bing"))
	}
}

func TestSearchStopsWhenTheCallerGivesUp(t *testing.T) {
	newStubEngines(t, map[string]http.HandlerFunc{
		"/bing": func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 1600*time.Millisecond)
	defer cancel()

	start := time.Now()
	resp, err := Search(ctx, "harga cabai rawit")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the caller's deadline (attempts %s)", err, outcomes(resp))
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("search outlived the caller's deadline: %v", time.Since(start))
	}
}

func TestSearchSkipsEnginesTheDeadlineCannotFit(t *testing.T) {
	newStubEngines(t, map[string]http.HandlerFunc{
		"/bing": func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(700 * time.Millisecond)
			fmt.Fprint(w, bingSoftBlock)
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	resp, err := Search(ctx, "harga cabai rawit")
	if err == nil {
		t.Fatal("want an error: the chain did not finish")
	}
	if got := outcomes(resp); !strings.HasPrefix(got, "bing=empty,duckduckgo=no_time") {
		t.Errorf("attempts = %s, want the engines after bing skipped for lack of time", got)
	}
}

func TestSearchRejectsBlankQuery(t *testing.T) {
	newStubEngines(t, nil)

	if _, err := Search(context.Background(), "   "); !errors.Is(err, ErrEmptyQuery) {
		t.Fatalf("err = %v, want ErrEmptyQuery", err)
	}
}

func TestGateSpacesRequests(t *testing.T) {
	g := &gate{interval: 150 * time.Millisecond}

	start := time.Now()
	for range 3 {
		if err := g.wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(start); elapsed < 290*time.Millisecond {
		t.Errorf("three requests took %v, want at least two intervals", elapsed)
	}

	g.coolDown(time.Minute, ErrBlocked)
	var cooling *coolingDownError
	if err := g.wait(context.Background()); !errors.As(err, &cooling) || !errors.Is(err, ErrBlocked) {
		t.Errorf("err = %v, want a cool-down that remembers the block", err)
	}
}

func TestCookiesArePrivateAndNeverWipedByAnEmptyJar(t *testing.T) {
	newStubEngines(t, map[string]http.HandlerFunc{
		"/bing": func(w http.ResponseWriter, r *http.Request) {
			if _, err := r.Cookie("MUID"); err != nil {
				http.SetCookie(w, &http.Cookie{Name: "MUID", Value: "abc", Path: "/", Expires: time.Now().Add(time.Hour)})
			}
			fmt.Fprint(w, bingPage(algoBlock(bingRedirect("https://www.bmkg.go.id/cuaca"), "Cuaca Bandung", "Prakiraan cuaca Bandung.")))
		},
	})

	if _, err := Search(context.Background(), "cuaca bandung"); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(CookieDir, "cookies.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("cookie file not written: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("cookie file mode = %o, want 600", perm)
	}

	before, _ := os.ReadFile(path)
	if !strings.Contains(string(before), "MUID") {
		t.Fatalf("cookie file = %s", before)
	}
}

// Found in review before v1.3.0 was tagged.

func TestSearchEngineTimeoutIsNotTheCallersDeadline(t *testing.T) {
	newStubEngines(t, map[string]http.HandlerFunc{
		"/bing": func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		},
		"/ddg/html/": html200(`<html><body><div class="no-results">No results.</div></body></html>`),
		"/news":      status(http.StatusOK, `<?xml version="1.0"?><rss version="2.0"><channel></channel></rss>`),
		"/w/api.php": status(http.StatusOK, `{"query":{"search":[]}}`),
	})
	AttemptTimeout = 300 * time.Millisecond

	resp, err := Search(context.Background(), "harga cabai rawit")
	if err == nil {
		t.Fatal("want bing's timeout reported")
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Errorf("err = %v unwraps to a context error while the caller's context is alive (attempts %s)", err, outcomes(resp))
	}
}

func TestSearchReportsARejectedKeyAsItselfNotAsABlock(t *testing.T) {
	newStubEngines(t, map[string]http.HandlerFunc{
		"/brave":     status(http.StatusPaymentRequired, `{"error":"quota"}`),
		"/bing":      html200(bingSoftBlock),
		"/ddg/html/": html200(`<html><body><div class="no-results">No results.</div></body></html>`),
		"/news":      status(http.StatusOK, `<?xml version="1.0"?><rss version="2.0"><channel></channel></rss>`),
		"/w/api.php": status(http.StatusOK, `{"query":{"search":[]}}`),
	})
	BraveAPIKey = "test-key"

	for round := range 2 {
		resp, err := Search(context.Background(), fmt.Sprintf("query kosong %d", round))
		if errors.Is(err, ErrBlocked) {
			t.Errorf("round %d: err = %v, want the quota error, not a block (attempts %s)", round, err, outcomes(resp))
		}
		if err == nil || !strings.Contains(err.Error(), "kuota habis") || strings.Count(err.Error(), "brave:") != 1 {
			t.Errorf("round %d: err = %v, want the quota named once by its engine", round, err)
		}
	}
}
