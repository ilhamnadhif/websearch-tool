package websearch

import (
	"strings"
	"sync"
	"time"
)

// CacheTTL is how long Search remembers an answer for the same query, so a
// model that repeats a search, or several users asking the same thing, do
// not each cost a round of requests that the engines count against us. Zero
// disables the cache. Empty answers are never cached: an engine that came
// back empty may answer a minute later.
var CacheTTL = 10 * time.Minute

const cacheCapacity = 256

type cacheEntry struct {
	resp    Response
	expires time.Time
}

type responseCache struct {
	mu      sync.Mutex
	entries map[string]cacheEntry
}

var searchCache = &responseCache{entries: map[string]cacheEntry{}}

// cacheKey normalizes the query and binds the answer to the engine
// configuration that produced it.
func cacheKey(query string) string {
	brave := "0"
	if BraveAPIKey != "" {
		brave = "1"
	}

	return strings.Join(strings.Fields(strings.ToLower(query)), " ") +
		"\x00" + strings.Join(Engines, ",") + "\x00" + brave
}

func (c *responseCache) get(key string) (Response, bool) {
	if CacheTTL <= 0 {
		return Response{}, false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[key]
	if !ok {
		return Response{}, false
	}
	if time.Now().After(entry.expires) {
		delete(c.entries, key)
		return Response{}, false
	}

	return entry.resp.clone(), true
}

func (c *responseCache) put(key string, resp Response) {
	if CacheTTL <= 0 || len(resp.Results) == 0 {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()

	if len(c.entries) >= cacheCapacity {
		for k, entry := range c.entries {
			if now.After(entry.expires) {
				delete(c.entries, k)
			}
		}
	}

	if len(c.entries) >= cacheCapacity {
		oldestKey := ""
		var oldest time.Time
		for k, entry := range c.entries {
			if oldestKey == "" || entry.expires.Before(oldest) {
				oldestKey, oldest = k, entry.expires
			}
		}
		delete(c.entries, oldestKey)
	}

	c.entries[key] = cacheEntry{resp: resp.clone(), expires: now.Add(CacheTTL)}
}

func (c *responseCache) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries = map[string]cacheEntry{}
}

func (r Response) clone() Response {
	r.Results = append([]Result(nil), r.Results...)
	r.Attempts = append([]Attempt(nil), r.Attempts...)
	return r
}
