package websearch

import (
	"context"
	"encoding/json"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// WikipediaEndpoint is the MediaWiki API that Search's last engine queries.
// Wikipedia answers datacenter IPs, but only when the client names itself:
// requests carry HonestUserAgent. Override it for another language edition,
// or in tests.
var WikipediaEndpoint = "https://id.wikipedia.org/w/api.php"

const wikipediaResultLimit = "5"

var htmlTagPattern = regexp.MustCompile(`<[^>]*>`)

type wikipediaSearch struct {
	Query struct {
		Search []struct {
			Title   string `json:"title"`
			Snippet string `json:"snippet"`
		} `json:"search"`
	} `json:"query"`
	Error *struct {
		Code string `json:"code"`
		Info string `json:"info"`
	} `json:"error"`
}

func searchWikipedia(ctx context.Context, query string) ([]Result, error) {
	params := url.Values{
		"action":        {"query"},
		"list":          {"search"},
		"format":        {"json"},
		"formatversion": {"2"},
		"utf8":          {"1"},
		"srlimit":       {wikipediaResultLimit},
		"srprop":        {"snippet"},
		"srsearch":      {query},
	}

	p, err := fetch(ctx, WikipediaEndpoint+"?"+params.Encode(), requestOptions{
		header: http.Header{
			"Accept":         {"application/json"},
			"Api-User-Agent": {HonestUserAgent},
		},
	})
	if err != nil {
		return nil, err
	}

	switch p.status {
	case http.StatusOK:
	case http.StatusTooManyRequests:
		return nil, ErrBlocked
	default:
		return nil, statusError(p.status)
	}

	var body wikipediaSearch
	if err := json.Unmarshal(p.body, &body); err != nil {
		return nil, ErrResultsNotParsed
	}
	if body.Error != nil {
		if body.Error.Code == "ratelimited" || body.Error.Code == "maxlag" {
			return nil, ErrBlocked
		}
		return nil, &engineError{msg: "API: " + body.Error.Code}
	}

	base := wikipediaArticleBase()

	var results []Result
	for _, hit := range body.Query.Search {
		title := strings.TrimSpace(hit.Title)
		if title == "" {
			continue
		}

		link := base + url.PathEscape(strings.ReplaceAll(title, " ", "_"))

		results = append(results, Result{
			Rank:    len(results) + 1,
			Title:   title,
			URL:     link,
			Site:    getDomain(link),
			Snippet: cleanText(html.UnescapeString(htmlTagPattern.ReplaceAllString(hit.Snippet, ""))),
		})
	}

	return results, nil
}

// wikipediaArticleBase derives the article URL prefix from the API endpoint:
// https://id.wikipedia.org/w/api.php serves https://id.wikipedia.org/wiki/.
func wikipediaArticleBase() string {
	u, err := url.Parse(WikipediaEndpoint)
	if err != nil || u.Host == "" {
		return "https://id.wikipedia.org/wiki/"
	}

	return u.Scheme + "://" + u.Host + "/wiki/"
}
