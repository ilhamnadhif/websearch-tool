// Package websearch searches the web over plain HTTP and returns ranked
// results (title, URL, domain, snippet) without fetching the pages
// themselves. Use package fetchpage to read a result's page.
//
// Search asks a chain of engines in turn and returns the first answer that is
// actually about the query: Brave Search when an API key is set, then Bing,
// DuckDuckGo, Bing News, and Wikipedia. Scraped engines fail in two quiet
// ways that look like answers. A datacenter IP often gets Bing's "no
// results" page whatever it asks, and some networks get a full page of
// unrelated decoy links. Search treats both as a miss and moves on, so an
// empty Response means every engine came back empty, not just the first.
package websearch

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const resultLimit = 10

// Engine names, as used in Engines, Response.Engine and Attempt.Engine.
const (
	EngineBrave      = "brave"
	EngineBing       = "bing"
	EngineDuckDuckGo = "duckduckgo"
	EngineBingNews   = "bing-news"
	EngineWikipedia  = "wikipedia"
)

// Engines is the order Search asks the engines in. Brave is skipped while
// BraveAPIKey is empty, and unknown names are ignored. Like the other
// package settings, set it once at startup, before the first Search.
var Engines = []string{EngineBrave, EngineBing, EngineDuckDuckGo, EngineBingNews, EngineWikipedia}

// AttemptTimeout bounds one engine's request, so an engine that hangs leaves
// time for the rest of the chain. The caller's context still bounds the
// whole search.
var AttemptTimeout = 6 * time.Second

// Outcomes of one engine attempt, as reported in Attempt.Outcome.
const (
	OutcomeOK        = "ok"         // relevant results; Search returned them
	OutcomeEmpty     = "empty"      // no results (possibly a soft block)
	OutcomeUnrelated = "unrelated"  // results about something else; discarded
	OutcomeBlocked   = "blocked"    // challenge or rate limit; the engine now cools down
	OutcomeCooldown  = "cooldown"   // skipped: still cooling down from a block
	OutcomeNoTime    = "no_time"    // skipped: too little of the deadline left
	OutcomeNotParsed = "not_parsed" // page did not parse (markup changed?)
	OutcomeError     = "error"      // network or HTTP failure
)

// ErrBlocked means at least one engine refused with a challenge or a rate
// limit and none of the others found anything. It is transient: back off and
// retry after a few minutes.
var ErrBlocked = errors.New("diblokir sementara oleh mesin pencari (captcha/rate limit) - coba lagi beberapa menit lagi")

// ErrResultsNotParsed means an engine answered but its page matched neither
// its result markup nor its "no results" notice, and no other engine found
// anything. It signals a markup change, not an empty query.
var ErrResultsNotParsed = errors.New("hasil pencarian tidak bisa di-parse (kemungkinan struktur halaman berubah)")

// ErrEmptyQuery means the query was blank.
var ErrEmptyQuery = errors.New("query pencarian kosong")

var (
	errNoEngines = errors.New("tidak ada mesin pencari yang aktif")
	errTimeShort = errors.New("waktu habis sebelum semua mesin pencari sempat dicoba")
)

// Result is a single search result entry.
type Result struct {
	Rank    int    `json:"rank"`
	Title   string `json:"title"`
	URL     string `json:"url"`
	Site    string `json:"site"`
	Snippet string `json:"snippet"`
	// Published is the article's date when the engine reports one (Bing
	// News, Brave). It is zero otherwise; a date may still appear in the
	// snippet's text.
	Published time.Time `json:"published,omitzero"`
}

// Attempt records what one engine did during a Search.
type Attempt struct {
	Engine     string `json:"engine"`
	Outcome    string `json:"outcome"`
	Results    int    `json:"results"`
	DurationMS int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
}

// Response is the outcome of one Search call.
type Response struct {
	Query   string   `json:"query"`
	Results []Result `json:"results"`
	// Engine names the engine whose results these are; empty when none had
	// any.
	Engine string `json:"engine,omitempty"`
	// Cached reports that the answer came from the in-memory cache (see
	// CacheTTL) rather than from the engines just now.
	Cached bool `json:"cached,omitempty"`
	// Attempts lists the engines asked, in order. It names no query text,
	// so it is safe to log.
	Attempts []Attempt `json:"attempts,omitempty"`
}

// engineError carries what the chain should do after an engine failed: count
// it as a block, and how long to leave the engine alone.
type engineError struct {
	msg      string
	blocked  bool
	cooldown time.Duration
}

func (e *engineError) Error() string { return e.msg }

func (e *engineError) Unwrap() error {
	if e.blocked {
		return ErrBlocked
	}
	return nil
}

type engine struct {
	name     string
	gate     *gate
	cooldown time.Duration // how long a challenge keeps the engine out
	strict   bool          // serves decoys: judge relevance strictly (see filterRelevant)
	enabled  func() bool
	search   func(context.Context, string) ([]Result, error)
}

// The pacing intervals keep bursts (a model firing several searches in one
// step) under each engine's patience. DuckDuckGo's is the long one: it
// challenges a second request that follows the first too closely.
var engines = map[string]*engine{
	EngineBrave: {
		name: EngineBrave, gate: &gate{interval: 1100 * time.Millisecond}, cooldown: time.Minute,
		enabled: func() bool { return BraveAPIKey != "" }, search: searchBrave,
	},
	EngineBing: {
		name: EngineBing, gate: &gate{interval: 500 * time.Millisecond}, cooldown: 10 * time.Minute,
		strict: true, search: searchBing,
	},
	EngineDuckDuckGo: {
		name: EngineDuckDuckGo, gate: &gate{interval: 2500 * time.Millisecond}, cooldown: 10 * time.Minute,
		search: searchDuckDuckGo,
	},
	EngineBingNews: {
		name: EngineBingNews, gate: &gate{interval: 500 * time.Millisecond}, cooldown: 5 * time.Minute,
		search: searchBingNews,
	},
	EngineWikipedia: {
		name: EngineWikipedia, gate: &gate{interval: 200 * time.Millisecond}, cooldown: time.Minute,
		search: searchWikipedia,
	},
}

// Search returns up to 10 results for query from the first engine in
// Engines whose answer is about the query. ctx bounds the whole chain;
// AttemptTimeout bounds each engine within it.
//
// The error is nil whenever results were found, and also when every engine
// answered without finding anything relevant: that is a genuinely empty
// result. Otherwise, check it with errors.Is:
//   - ErrBlocked: an engine refused (challenge or rate limit); retry later
//   - ErrResultsNotParsed: an engine's page did not parse (markup change)
//   - the context's error, when ctx ended first
//
// Response.Attempts says what each engine did, in either case.
func Search(ctx context.Context, query string) (Response, error) {
	query = strings.TrimSpace(query)
	resp := Response{Query: query}

	if query == "" {
		return resp, ErrEmptyQuery
	}

	key := cacheKey(query)
	if cached, ok := searchCache.get(key); ok {
		cached.Query = query
		cached.Cached = true
		return cached, nil
	}

	chain := activeEngines()
	if len(chain) == 0 {
		return resp, errNoEngines
	}

	var (
		blocked   bool
		notParsed bool
		lastErr   error
	)

	for _, e := range chain {
		if err := ctx.Err(); err != nil {
			return resp, err
		}

		attempt, results, err := e.attempt(ctx, query)
		resp.Attempts = append(resp.Attempts, attempt)

		switch attempt.Outcome {
		case OutcomeOK:
			resp.Results = results
			resp.Engine = e.name
			searchCache.put(key, resp)
			return resp, nil
		case OutcomeBlocked:
			blocked = true
		case OutcomeCooldown:
			// Skipped for an earlier refusal: a block counts as one; a
			// rejected API key or an exhausted quota is reported as itself.
			if errors.Is(err, ErrBlocked) {
				blocked = true
			} else {
				lastErr = err
			}
		case OutcomeNotParsed:
			notParsed = true
		case OutcomeNoTime:
			lastErr = errTimeShort
		case OutcomeError:
			lastErr = err
		}
	}

	if err := ctx.Err(); err != nil {
		return resp, err
	}

	switch {
	case blocked:
		return resp, ErrBlocked
	case notParsed:
		return resp, ErrResultsNotParsed
	default:
		return resp, lastErr
	}
}

func activeEngines() []*engine {
	seen := map[string]bool{}

	var chain []*engine
	for _, name := range Engines {
		e, ok := engines[name]
		if !ok || seen[name] {
			continue
		}
		seen[name] = true

		if e.enabled != nil && !e.enabled() {
			continue
		}
		chain = append(chain, e)
	}

	return chain
}

// attempt asks one engine, after its gate lets the request through, and
// judges the answer.
func (e *engine) attempt(ctx context.Context, query string) (a Attempt, results []Result, err error) {
	a.Engine = e.name
	start := time.Now()
	defer func() { a.DurationMS = time.Since(start).Milliseconds() }()

	if err = e.gate.wait(ctx); err != nil {
		var cooling *coolingDownError
		switch {
		case errors.As(err, &cooling):
			a.Outcome = OutcomeCooldown
			err = fmt.Errorf("%s: %w", e.name, err)
		case errors.Is(err, errNoTimeLeft):
			a.Outcome = OutcomeNoTime
		default:
			a.Outcome = OutcomeError
		}
		a.Error = err.Error()
		return a, nil, err
	}

	attemptCtx, cancel := context.WithTimeout(ctx, AttemptTimeout)
	results, err = e.search(attemptCtx, query)
	cancel()

	// An engine that ran out of its own AttemptTimeout while the caller's
	// context is still alive must not read as the caller's deadline: an
	// error wrapping context.DeadlineExceeded makes callers report that the
	// whole search timed out.
	if err != nil && ctx.Err() == nil && attemptCtx.Err() != nil && errors.Is(err, attemptCtx.Err()) {
		err = fmt.Errorf("tidak menjawab dalam %v", AttemptTimeout)
	}

	var engErr *engineError
	if errors.As(err, &engErr) && engErr.cooldown > 0 {
		e.gate.coolDown(engErr.cooldown, err)
	} else if errors.Is(err, ErrBlocked) {
		e.gate.coolDown(e.cooldown, err)
	}

	switch {
	case errors.Is(err, ErrBlocked):
		a.Outcome = OutcomeBlocked
	case errors.Is(err, ErrResultsNotParsed):
		a.Outcome = OutcomeNotParsed
	case err != nil:
		a.Outcome = OutcomeError
		err = fmt.Errorf("%s: %w", e.name, err)
	case len(results) == 0:
		a.Outcome = OutcomeEmpty
		return a, nil, nil
	default:
		a.Results = len(results)

		kept, related := filterRelevant(query, results, e.strict)
		if !related {
			a.Outcome = OutcomeUnrelated
			return a, nil, nil
		}

		a.Outcome = OutcomeOK
		a.Results = len(kept)
		return a, kept, nil
	}

	a.Error = err.Error()
	return a, nil, err
}

// resetState clears the cache and every engine's pacing and cool-down. Tests
// use it; nothing else needs to.
func resetState() {
	searchCache.reset()
	for _, e := range engines {
		e.gate.reset()
	}
}

func cleanText(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func getDomain(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}

	return strings.TrimPrefix(u.Hostname(), "www.")
}
