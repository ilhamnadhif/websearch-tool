# websearch-tool

A small Go toolkit that gives an LLM two tool-calling capabilities: **searching the web** and **reading a page**. It ships as two importable packages (`websearch`, `fetchpage`) plus a reference orchestration example ([`example/orchestration/main.go`](example/orchestration/main.go)) that wires them into an OpenAI-compatible tool-calling loop via [OpenRouter](https://openrouter.ai/).

Everything runs over plain HTTP: no headless browser and no search server to host.

## Features

### `websearch.Search`: several engines, judged on content

`Search` asks a chain of engines in turn and returns the first answer that is actually about the query:

1. [Brave Search API](https://brave.com/search/api/), only when `BraveAPIKey` is set
2. Bing
3. DuckDuckGo (HTML endpoint; its text-only endpoint takes over if the HTML markup stops parsing)
4. Bing News (RSS)
5. Wikipedia (MediaWiki API)

Scraped engines fail quietly in ways that look like answers, and the chain is built around that:

- **Soft blocks.** From a datacenter IP, Bing often serves its "no results" page for any query. `Search` treats an empty page as a miss and asks the next engine; it does not report it as the final answer.
- **Decoys.** Some networks get a full page of links unrelated to the query from Bing. These are random pages, or pages about only the query's first word: "jadwal krl bogor" answered with prayer timetables. Each answer is checked against the query's words:
  - a result near the top must cover at least half of them;
  - generic words such as "harga", "jadwal" and "jakarta" count half;
  - for Bing, the specific words must also appear.
  
  An answer that fails is discarded, and results that don't cover a third of the words are dropped from one that passes.
- **Challenges.** A CAPTCHA, an HTTP 202 challenge or a rate limit puts that engine on a cool-down (10 minutes for DuckDuckGo and Bing), and later searches skip it rather than prolong the block.
- **Pacing.** Each engine has its own request spacing (2.5 s for DuckDuckGo, which challenges back-to-back requests). A slow engine never delays the others, and an engine is skipped when the caller's deadline leaves too little time for it.

Other details:
- Answers are cached in memory for 10 minutes (`CacheTTL`). Empty answers are not cached.
- Results are Indonesian-first by default: `mkt=id-ID` on Bing, `kl=id-id` on DuckDuckGo, country `ID` on Brave, and id.wikipedia.org. Each is an exported variable you can change for another market.
- `Response.Attempts` records what every engine did (outcome, result count, duration) and contains no query text, so it is safe to log.

### `fetchpage`: readable text that keeps its structure

- **Main text.**
  - Article pages go through [go-readability](https://codeberg.org/readeck/go-readability).
  - Listings, tables and forms fall back to the whole page without its navigation.
  - Data tables that readability dropped are added back, because tables hold what people ask about: prices, rates, schedules.
- **Structure.** Content keeps the page's shape in plain text: `## ` starts a heading, `- ` starts a list item, and table cells are joined by ` | `.
- **Structured data first.** JSON-LD facts lead the content: product prices and availability, events, opening hours, FAQ answers and recipes. `Result.Published` carries the publication date when the page states one.
- **Focus.** With `Options.Focus` set (e.g. the user's question), a page longer than `Options.MaxChars` (default 4000) returns the passages that best match it, ranked by how rare each word is on the page. Passages stay in page order, and `…` marks the text in between that was left out. A row picked from the middle of a table brings its column names along.
- **Refusals.**
  - A site that refuses the browser-like request (HTTP 202, 403, 429, or a Cloudflare challenge) is asked once more under `HonestUserAgent`.
  - A second refusal returns `ErrBlocked`.
  - A page whose text needs JavaScript returns its description (`Source: "summary"`) or `ErrNoContent`, never an empty success.
- **Formats and sites.**
  - PDFs are read through Poppler's `pdftotext` (`PDFToText` is replaceable); plain text is supported; legacy charsets such as windows-1252 are converted.
  - MSN articles are read through MSN's content API; the article pages need JavaScript.
  - Wikipedia articles are read through Wikipedia's API, which datacenter IPs can reach.
- **Cache.** Fetched pages are cached for 10 minutes, so a follow-up with another focus does not download the page again.

### Security

- `fetchpage` refuses loopback, private, link-local (including cloud metadata endpoints), unspecified and multicast addresses. It checks the resolved IP at dial time, so neither DNS rebinding nor a redirect can get around the check.
- Bodies over 5 MB (15 MB for a PDF) are rejected before parsing.
- `FetchMany` caps a call at 5 URLs.

## Requirements

- Go 1.26+
- Optional: `pdftotext` (Poppler) on `PATH`, to read PDFs
- Optional: a [Brave Search API](https://brave.com/search/api/) key
- An [OpenRouter](https://openrouter.ai/keys) API key, only to run the orchestration example

## Install

```bash
go get github.com/ilhamnadhif/websearch-tool
```

## Usage

The two packages are independent; import only what you need.

### `websearch`

```go
import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ilhamnadhif/websearch-tool/websearch"
)

// Once, at startup:
websearch.HonestUserAgent = "MyBot/1.0 (+https://example.com; ops@example.com)"
websearch.BraveAPIKey = os.Getenv("BRAVE_SEARCH_API_KEY") // optional

ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
defer cancel()

resp, err := websearch.Search(ctx, "harga emas antam hari ini")
switch {
case errors.Is(err, websearch.ErrBlocked):
	// An engine refused (challenge or rate limit) and no other found anything.
case errors.Is(err, websearch.ErrResultsNotParsed):
	// An engine's markup changed and no other found anything.
case err == nil && len(resp.Results) == 0:
	// Every engine answered and found nothing relevant: a real empty result.
}

for _, r := range resp.Results {
	fmt.Println(r.Rank, r.Title, r.URL, r.Site, r.Snippet, r.Published)
}

for _, a := range resp.Attempts { // e.g. bing=empty duckduckgo=ok
	fmt.Println(a.Engine, a.Outcome, a.Results, a.DurationMS)
}
```

### `fetchpage`

```go
import (
	"context"
	"fmt"
	"time"

	"github.com/ilhamnadhif/websearch-tool/fetchpage"
)

ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
defer cancel()

// The opening 4000 characters, as before:
title, content, err := fetchpage.Fetch(ctx, "https://example.com/article")

// The parts of a long page that answer a question:
result, err := fetchpage.FetchWithOptions(ctx, "https://www.bi.go.id/id/statistik/informasi-kurs/transaksi-bi/default.aspx",
	fetchpage.Options{Focus: "kurs jual USD", MaxChars: 3000})
fmt.Println(result.Title, result.Source, result.Published)
fmt.Println(result.Content) // "... Mata Uang | Nilai | Kurs Jual | Kurs Beli\nUSD | 1 | ..."

// Several URLs concurrently (capped at 5 per call):
results := fetchpage.FetchManyWithOptions(ctx, []string{
	"https://example.com/a",
	"https://example.com/b",
}, fetchpage.Options{Focus: "jam buka"})
for _, r := range results {
	if r.Error != "" {
		continue
	}
	fmt.Println(r.Title, len(r.Content))
}
```

## Orchestration example

[`example/orchestration/main.go`](example/orchestration/main.go) is a runnable, self-contained example that combines `websearch` and `fetchpage` into an LLM tool-calling loop. It is a reference implementation; you don't need it to use the packages.

```bash
git clone git@github.com:ilhamnadhif/websearch-tool.git
cd websearch-tool
```

Open `example/orchestration/main.go` and set your OpenRouter API key:

```go
const openRouterAPIKey = "sk-or-v1-..."
```

By default it targets `deepseek/deepseek-v4-flash-0731` routed through the `DeepInfra` provider. Both are constants near the top of the file.

Then build and run:

```bash
go build -o websearch-tool ./example/orchestration
./websearch-tool "berapa kurs dollar hari ini?"
```

The example logs to stderr:
- each tool call;
- which engines a search asked;
- the number of model calls.

The final answer goes to stdout:

```
[model] panggilan API ke-1
[tool] web_search({"query": "kurs dollar rupiah hari ini"})
[search] bing=empty duckduckgo=ok
[model] panggilan API ke-2
[tool] fetch_page({"urls": ["https://www.bi.go.id/..."], "focus": "kurs USD"})
[model] panggilan API ke-3
[ringkasan] 3 panggilan model API, 2 tool call
Kurs jual USD di Bank Indonesia hari ini Rp16.4xx...
```

## How it works

```
example/orchestration/main.go   an OpenAI-compatible tool-calling loop
websearch/                      tool "web_search": engine chain -> []Result
  websearch.go                  the chain, outcomes, errors
  bing.go duckduckgo.go         scraped engines
  bingnews.go wikipedia.go      feed and API engines
  brave.go                      Brave Search API (optional)
  relevance.go                  decoy detection
  http.go cache.go              pacing, cool-downs, cookies, cache
fetchpage/                      tool "fetch_page": URL -> title + structured text
  fetchpage.go                  fetching, SSRF guard, refusals, charsets
  render.go                     DOM -> headings, items, table rows
  structured.go                 JSON-LD and meta tags
  focus.go                      passage selection
  sources.go pdf.go             MSN, Wikipedia, PDF, plain text
internal/textmatch/             word matching shared by both
```

## Known limitations

- Bing and DuckDuckGo are scraped. A markup change shows up as `ErrResultsNotParsed` (when no other engine answers) rather than as silence. Heavy use still earns challenges; a Brave API key avoids depending on them.
- Pages that render their text with JavaScript return only their description (`Source: "summary"`). MSN and Wikipedia are the exceptions, because they are read through their APIs.
- Some sites refuse every automated client, Cloudflare-protected ones in particular. They return `ErrBlocked`; read another source.
- Only the first 40 pages of a PDF are read, and a scanned PDF without a text layer returns `ErrNoContent`.

## License

[MIT](LICENSE)
