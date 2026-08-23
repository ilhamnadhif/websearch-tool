# websearch-tool

A small Go toolkit that gives an LLM two tool-calling capabilities: **searching the web** and **reading a page's full content**. It ships as two importable packages (`websearch`, `fetchpage`) plus a reference CLI (`main.go`) that wires them into an OpenAI-compatible tool-calling loop via [OpenRouter](https://openrouter.ai/).

## Features

- `websearch.Search` — queries Bing over plain HTTP (no headless browser) and returns up to 10 results (title, URL, domain, snippet).
- `fetchpage.Fetch` / `fetchpage.FetchMany` — downloads one or more URLs in parallel and extracts clean article text (via [go-readability](https://codeberg.org/readeck/go-readability), with a manual strip-tag fallback for non-article pages like listings).
- SSRF-safe by default: `fetchpage` refuses to connect to loopback, private, link-local (including cloud metadata endpoints), or unspecified/multicast addresses, and validates the resolved IP at dial time so redirects can't bypass the check.
- Both packages accept a `context.Context`, so callers control cancellation and deadlines instead of relying on fixed internal timeouts.
- A reference CLI demonstrates a full tool-calling loop against an OpenAI-compatible chat API (OpenRouter + DeepSeek by default), with retry-on-transient-network-error and a graceful "answer with what you have" fallback when the turn/time budget runs out.

## Requirements

- Go 1.26+
- An [OpenRouter](https://openrouter.ai/keys) API key (only needed to run the reference CLI — the `websearch`/`fetchpage` packages work standalone without it)

## Install

```bash
go get github.com/ilhamnadhif/websearch-tool
```

## Usage

Both packages are independent of each other and of the CLI, so import only what you need.

### `websearch`

```go
import (
	"context"
	"fmt"
	"time"

	"github.com/ilhamnadhif/websearch-tool/websearch"
)

ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
defer cancel()

resp, err := websearch.Search(ctx, "golang context cancellation")
if err != nil {
	// handle error (e.g. temporarily blocked by Bing's anti-bot check)
}

for _, r := range resp.Results {
	fmt.Println(r.Rank, r.Title, r.URL, r.Site, r.Snippet)
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

ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
defer cancel()

title, content, err := fetchpage.Fetch(ctx, "https://example.com/article")

// or fetch several URLs concurrently (capped at 5 per call):
results := fetchpage.FetchMany(ctx, []string{
	"https://example.com/a",
	"https://example.com/b",
})
for _, r := range results {
	if r.Error != "" {
		continue
	}
	fmt.Println(r.Title, len(r.Content))
}
```

## Running the reference CLI

The CLI is a runnable demo of both packages wired into an LLM tool-calling loop — useful as a reference implementation, not required for using the packages themselves.

```bash
git clone git@github.com:ilhamnadhif/websearch-tool.git
cd websearch-tool
```

Open `main.go` and set your OpenRouter API key:

```go
const openRouterAPIKey = "sk-or-v1-..."
```

By default it targets `deepseek/deepseek-v4-flash-0731` routed through the `DeepInfra` provider — both are constants near the top of `main.go` if you want to change them.

Then build and run:

```bash
go build -o websearch-tool .
./websearch-tool "what's the weather in Kudus today?"
```

Tool calls and per-turn API call counts are logged to stderr; the final answer is printed to stdout:

```
[model] panggilan API ke-1
[tool] web_search({"query": "weather in Kudus today"})
[model] panggilan API ke-2
[tool] fetch_page({"urls": ["https://www.bmkg.go.id/..."]})
[model] panggilan API ke-3
[ringkasan] 3 panggilan model API, 2 tool call
Based on BMKG data, Kudus is expected to be sunny today with a high of ~33°C...
```

Unquoted multi-word questions also work — all CLI arguments are joined into a single question.

## How it works

```
main.go            reference CLI: orchestrates an OpenAI-compatible tool-calling loop
  ├─ websearch/     tool "web_search": Bing HTML scrape -> []Result
  └─ fetchpage/     tool "fetch_page": URL -> extracted title + content
```

The CLI's system prompt tells the model to always search before answering, which is useful for exercising the tools during testing but is not something the packages themselves enforce — an application importing `websearch`/`fetchpage` directly decides for itself when (or whether) to call them.

## Security notes

- `fetchpage` validates the destination IP at connection time (not just the hostname), so DNS rebinding and redirects to internal addresses are both blocked.
- Only `http`/`https` URLs are fetched.
- `FetchMany` caps requests at 5 URLs per call to limit how much fan-out a single tool call can trigger.

## Known limitations

- Search relies on scraping Bing's HTML (`li.b_algo` markup), which can break silently if Bing changes its page structure, and Bing may temporarily block requests with a CAPTCHA under heavy use.
- `fetchpage` only handles HTML pages — PDFs and other non-HTML content are rejected.
- Extracted content is truncated to 4000 characters (UTF-8 rune-safe).
