package websearch

import (
	"context"
	"encoding/json"
	"html"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// BraveEndpoint is the Brave Search web API. Override it only in tests.
var BraveEndpoint = "https://api.search.brave.com/res/v1/web/search"

// BraveAPIKey enables Brave Search as the first engine Search asks. Brave is
// an API rather than a scraped page: it does not turn datacenter IPs away
// and never serves decoys. Leave it empty to run on the scraped engines
// only.
var BraveAPIKey = ""

// BraveCountry and BraveSearchLang pin Brave's results to Indonesia and
// Indonesian.
var (
	BraveCountry    = "ID"
	BraveSearchLang = "id"
)

type braveSearch struct {
	Web struct {
		Results []struct {
			Title       string `json:"title"`
			URL         string `json:"url"`
			Description string `json:"description"`
			PageAge     string `json:"page_age"`
		} `json:"results"`
	} `json:"web"`
}

func searchBrave(ctx context.Context, query string) ([]Result, error) {
	params := url.Values{
		"q":           {query},
		"country":     {BraveCountry},
		"search_lang": {BraveSearchLang},
		"count":       {strconv.Itoa(resultLimit)},
	}

	p, err := fetch(ctx, BraveEndpoint+"?"+params.Encode(), requestOptions{
		header: http.Header{
			"Accept":               {"application/json"},
			"X-Subscription-Token": {BraveAPIKey},
		},
	})
	if err != nil {
		return nil, err
	}

	switch p.status {
	case http.StatusOK:
	case http.StatusTooManyRequests:
		return nil, &engineError{msg: "rate limit", blocked: true, cooldown: retryAfter(p.header, time.Minute)}
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, &engineError{msg: "API key ditolak (" + strconv.Itoa(p.status) + ")", cooldown: 10 * time.Minute}
	case http.StatusPaymentRequired:
		return nil, &engineError{msg: "kuota habis", cooldown: time.Hour}
	default:
		return nil, statusError(p.status)
	}

	var body braveSearch
	if err := json.Unmarshal(p.body, &body); err != nil {
		return nil, ErrResultsNotParsed
	}

	var results []Result
	for _, hit := range body.Web.Results {
		if len(results) >= resultLimit {
			break
		}
		if !isWebURL(hit.URL) {
			continue
		}

		results = append(results, Result{
			Rank:      len(results) + 1,
			Title:     cleanText(html.UnescapeString(htmlTagPattern.ReplaceAllString(hit.Title, ""))),
			URL:       hit.URL,
			Site:      getDomain(hit.URL),
			Snippet:   cleanText(html.UnescapeString(htmlTagPattern.ReplaceAllString(hit.Description, ""))),
			Published: parseBraveDate(hit.PageAge),
		})
	}

	return results, nil
}

func parseBraveDate(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// retryAfter reads a Retry-After header given in seconds, falling back to
// fallback when it is absent or unreadable.
func retryAfter(h http.Header, fallback time.Duration) time.Duration {
	if seconds, err := strconv.Atoi(strings.TrimSpace(h.Get("Retry-After"))); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return fallback
}
