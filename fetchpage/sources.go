package fetchpage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// Two sites are read through their APIs instead of their pages, because
// their pages yield nothing useful to a plain HTTP client: MSN renders its
// articles with JavaScript, and Wikipedia refuses browser-like requests from
// datacenter IPs while welcoming a client that names itself.

var (
	msnArticlePattern = regexp.MustCompile(`/ar-([A-Za-z0-9]+)`)
	msnLocalePattern  = regexp.MustCompile(`^[a-z]{2}-[a-z]{2}$`)

	// msnDetailEndpoint serves an MSN article as JSON: title, HTML body,
	// publication time and the original publisher's name.
	msnDetailEndpoint = "https://assets.msn.com/content/view/v2/Detail"

	// wikipediaAPI returns the MediaWiki API of a Wikipedia host. Tests
	// replace it.
	wikipediaAPI = func(host string) string { return "https://" + host + "/w/api.php" }
)

// msnArticle recognises https://www.msn.com/<locale>/.../ar-<id> links.
func msnArticle(u *url.URL) (locale, id string, ok bool) {
	host := strings.ToLower(u.Hostname())
	if host != "msn.com" && !strings.HasSuffix(host, ".msn.com") {
		return "", "", false
	}

	m := msnArticlePattern.FindStringSubmatch(u.Path)
	if m == nil {
		return "", "", false
	}

	locale = "en-us"
	if first, _, _ := strings.Cut(strings.Trim(strings.ToLower(u.Path), "/"), "/"); msnLocalePattern.MatchString(first) {
		locale = first
	}

	return locale, m[1], true
}

type msnDetail struct {
	Title             string `json:"title"`
	Abstract          string `json:"abstract"`
	Body              string `json:"body"`
	PublishedDateTime string `json:"publishedDateTime"`
	Provider          struct {
		Name string `json:"name"`
	} `json:"provider"`
}

func loadMSN(ctx context.Context, locale, id string) (*document, error) {
	resp, err := get(ctx, msnDetailEndpoint+"/"+url.PathEscape(locale)+"/"+url.PathEscape(id), false, "application/json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("msn: status %s", resp.Status)
	}

	body, err := readBody(resp, maxBodyBytes)
	if err != nil {
		return nil, err
	}

	var detail msnDetail
	if err := json.Unmarshal(body, &detail); err != nil {
		return nil, fmt.Errorf("msn: %w", err)
	}

	root, err := html.Parse(strings.NewReader(detail.Body))
	if err != nil {
		return nil, fmt.Errorf("msn: %w", err)
	}

	blocks := renderBlocks(root)
	if len(blocks) == 0 && detail.Abstract != "" {
		blocks = []block{{kind: blockText, text: cleanText(detail.Abstract)}}
	}
	if len(blocks) == 0 {
		return nil, ErrNoContent
	}

	doc := &document{
		title:     cleanText(detail.Title),
		published: parseDate(detail.PublishedDateTime),
		source:    SourceMSN,
		blocks:    blocks,
	}
	if provider := cleanText(detail.Provider.Name); provider != "" {
		doc.facts = []string{"Sumber asli: " + provider}
	}

	return doc, nil
}

// wikipediaArticle recognises https://<lang>.wikipedia.org/wiki/<Title>
// links, mobile ones included.
func wikipediaArticle(u *url.URL) (host, title string, ok bool) {
	host = strings.ToLower(u.Hostname())
	if !strings.HasSuffix(host, ".wikipedia.org") || !strings.HasPrefix(u.Path, "/wiki/") {
		return "", "", false
	}

	title = strings.ReplaceAll(strings.TrimPrefix(u.Path, "/wiki/"), "_", " ")
	if strings.TrimSpace(title) == "" {
		return "", "", false
	}

	return strings.Replace(host, ".m.wikipedia.org", ".wikipedia.org", 1), title, true
}

type wikipediaExtract struct {
	Query struct {
		Pages []struct {
			Title   string `json:"title"`
			Extract string `json:"extract"`
			Missing bool   `json:"missing"`
		} `json:"pages"`
	} `json:"query"`
}

func loadWikipedia(ctx context.Context, host, title string) (*document, error) {
	params := url.Values{
		"action":          {"query"},
		"prop":            {"extracts"},
		"explaintext":     {"1"},
		"exsectionformat": {"wiki"},
		"redirects":       {"1"},
		"format":          {"json"},
		"formatversion":   {"2"},
		"titles":          {title},
	}

	resp, err := get(ctx, wikipediaAPI(host)+"?"+params.Encode(), true, "application/json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("wikipedia: status %s", resp.Status)
	}

	body, err := readBody(resp, maxBodyBytes)
	if err != nil {
		return nil, err
	}

	var extract wikipediaExtract
	if err := json.Unmarshal(body, &extract); err != nil {
		return nil, fmt.Errorf("wikipedia: %w", err)
	}
	if len(extract.Query.Pages) == 0 || extract.Query.Pages[0].Missing {
		return nil, fmt.Errorf("wikipedia: artikel %q tidak ada", title)
	}

	page := extract.Query.Pages[0]
	blocks := wikiTextBlocks(page.Extract)
	if len(blocks) == 0 {
		return nil, ErrNoContent
	}

	return &document{title: page.Title, source: SourceWikipedia, blocks: blocks}, nil
}

var wikiHeadingPattern = regexp.MustCompile(`^(={2,6})\s*(.*?)\s*={2,6}$`)

// skippedWikiSections are the reference sections at the end of an article.
// They are long lists of citations that answer nothing.
var skippedWikiSections = map[string]bool{
	"referensi": true, "catatan": true, "catatan kaki": true, "pranala luar": true,
	"bacaan lanjutan": true, "daftar pustaka": true, "lihat pula": true, "lihat juga": true,
	"references": true, "notes": true, "external links": true, "further reading": true,
	"see also": true, "bibliography": true, "sources": true,
}

// wikiTextBlocks turns an explaintext extract into blocks: "== Sejarah =="
// lines become headings, every other non-empty line a paragraph.
func wikiTextBlocks(extract string) []block {
	var blocks []block
	skipping := false

	for _, line := range strings.Split(extract, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		if m := wikiHeadingPattern.FindStringSubmatch(line); m != nil {
			heading := cleanText(m[2])
			skipping = skippedWikiSections[strings.ToLower(heading)]
			if !skipping && heading != "" {
				blocks = append(blocks, block{kind: blockHeading, text: heading})
			}
			continue
		}

		if !skipping {
			blocks = append(blocks, splitLongText(cleanText(line))...)
		}
		if blocksLen(blocks) >= maxRenderedChars {
			break
		}
	}

	return dropEmptySections(blocks)
}

// dropEmptySections removes headings with nothing under them.
func dropEmptySections(blocks []block) []block {
	out := make([]block, 0, len(blocks))
	for i, b := range blocks {
		if b.kind == blockHeading && (i+1 == len(blocks) || blocks[i+1].kind == blockHeading) {
			continue
		}
		out = append(out, b)
	}
	return out
}

// splitLongText breaks a long paragraph at sentence ends into blocks of
// about passageChars characters, so focus selection can keep part of it.
func splitLongText(text string) []block {
	if text == "" {
		return nil
	}

	var blocks []block
	for len(text) > passageChars {
		cut := strings.LastIndex(text[:passageChars], ". ")
		if cut < passageChars/3 {
			cut = strings.LastIndex(text[:passageChars], " ")
		}
		if cut <= 0 {
			break
		}
		blocks = append(blocks, block{kind: blockText, text: strings.TrimSpace(text[:cut+1])})
		text = strings.TrimSpace(text[cut+1:])
	}

	if text != "" {
		blocks = append(blocks, block{kind: blockText, text: text})
	}
	return blocks
}

// textBlocks turns plain text (a PDF's, a text/plain page's) into blocks:
// blank lines separate paragraphs, and the lines of one paragraph, wrapped
// at the page's width, are joined back together.
func textBlocks(text string) []block {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\f", "\n\n")

	var blocks []block
	var paragraph bytes.Buffer
	size := 0

	flush := func() {
		for _, b := range splitLongText(cleanText(paragraph.String())) {
			blocks = append(blocks, b)
			size += len(b.text) + 1
		}
		paragraph.Reset()
	}

	// Like a rendered page, a text document stops at maxRenderedChars: far
	// more than any caller shows, and the cache must not hold a whole book.
	for _, line := range strings.Split(text, "\n") {
		if size >= maxRenderedChars {
			break
		}
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		paragraph.WriteString(line)
		paragraph.WriteByte(' ')
	}
	flush()

	return blocks
}

// firstLineTitle picks a title for a document without one: its first line,
// when that is short enough to be a title.
func firstLineTitle(blocks []block) string {
	if len(blocks) == 0 {
		return ""
	}
	if first := blocks[0].text; len([]rune(first)) <= 150 {
		return first
	}
	return ""
}

func firstTime(times ...time.Time) time.Time {
	for _, t := range times {
		if !t.IsZero() {
			return t
		}
	}
	return time.Time{}
}
