package fetchpage

import (
	"encoding/json"
	"html"
	"regexp"
	"strconv"
	"strings"
	"time"

	nethtml "golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

const (
	maxStructuredObjects = 60
	maxFacts             = 12
)

var tagPattern = regexp.MustCompile(`<[^>]*>`)

// pageMeta is what a page's head says about it.
type pageMeta struct {
	title       string
	description string
	published   time.Time
}

// publishedMetaKeys are the meta names, properties and itemprops that carry
// a publication date, lower-cased. Indonesian news sites use several of the
// non-standard ones (content_PublishedDate, publishdate).
var publishedMetaKeys = map[string]bool{
	"article:published_time": true, "og:published_time": true, "datepublished": true,
	"pubdate": true, "publishdate": true, "publish-date": true, "publish_date": true,
	"date": true, "dc.date": true, "dc.date.issued": true, "dcterms.created": true,
	"content_publisheddate": true, "publication_date": true, "sailthru.date": true,
	"parsely-pub-date": true, "article.published": true, "release_date": true,
}

func readMeta(root *nethtml.Node) pageMeta {
	var m pageMeta
	var docTitle, ogTitle, ogDescription, description string

	var walk func(*nethtml.Node)
	walk = func(n *nethtml.Node) {
		if n.Type == nethtml.ElementNode {
			switch n.DataAtom {
			case atom.Title:
				if docTitle == "" {
					docTitle = textContent(n)
				}
			case atom.Meta:
				key, content := metaKeyContent(n)
				switch {
				case key == "og:title":
					ogTitle = content
				case key == "og:description":
					ogDescription = content
				case key == "description":
					description = content
				case publishedMetaKeys[key] && m.published.IsZero():
					m.published = parseDate(content)
				}
			case atom.Script, atom.Style, atom.Svg:
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)

	m.title = firstNonEmpty(ogTitle, docTitle)
	m.description = firstNonEmpty(ogDescription, description)

	return m
}

func metaKeyContent(n *nethtml.Node) (key, content string) {
	for _, a := range n.Attr {
		switch strings.ToLower(a.Key) {
		case "name", "property", "itemprop":
			if key == "" {
				key = strings.ToLower(strings.TrimSpace(a.Val))
			}
		case "content":
			content = cleanText(a.Val)
		}
	}
	return key, content
}

// structured is what a page's JSON-LD says, rendered as text: short facts
// (a product's price, an office's opening hours) shown before the page
// text, and longer sections (FAQ answers, a recipe) appended to it.
type structured struct {
	facts     []string
	blocks    []block
	title     string
	published time.Time
}

func readStructured(root *nethtml.Node) structured {
	var s structured

	for _, obj := range jsonLDObjects(root) {
		types := typesOf(obj)

		switch {
		case types["product"] || types["productgroup"] || types["car"] || types["vehicle"]:
			s.addFact(productFact(obj))
		case hasTypeSuffix(types, "event"):
			s.addFact(eventFact(obj))
		case types["faqpage"]:
			s.blocks = append(s.blocks, faqBlocks(obj)...)
		case types["recipe"]:
			s.blocks = append(s.blocks, recipeBlocks(obj)...)
		case hasTypeSuffix(types, "article") || types["blogposting"] || types["report"]:
			if s.published.IsZero() {
				s.published = parseDate(text(obj["datePublished"]))
			}
			if s.title == "" {
				s.title = text(obj["headline"])
			}
		}

		if obj["openingHours"] != nil || obj["openingHoursSpecification"] != nil {
			s.addFact(placeFact(obj))
		}
	}

	return s
}

func (s *structured) addFact(fact string) {
	if fact == "" || len(s.facts) >= maxFacts {
		return
	}
	for _, f := range s.facts {
		if f == fact {
			return
		}
	}
	s.facts = append(s.facts, fact)
}

// jsonLDObjects returns every JSON-LD object on the page, @graph members and
// array elements included.
func jsonLDObjects(root *nethtml.Node) []map[string]any {
	var found []map[string]any

	var collect func(any)
	collect = func(v any) {
		if len(found) >= maxStructuredObjects {
			return
		}
		switch v := v.(type) {
		case []any:
			for _, item := range v {
				collect(item)
			}
		case map[string]any:
			found = append(found, v)
			if graph, ok := v["@graph"]; ok {
				collect(graph)
			}
			// A listing page lists its products in an ItemList, each
			// element wrapping the product in "item".
			for _, element := range objects(v["itemListElement"]) {
				if item, ok := element["item"].(map[string]any); ok {
					collect(item)
				}
			}
		}
	}

	var walk func(*nethtml.Node)
	walk = func(n *nethtml.Node) {
		if n.Type == nethtml.ElementNode && n.DataAtom == atom.Script && isJSONLD(n) {
			raw := strings.TrimSpace(textContent(n))
			raw = strings.TrimSuffix(strings.TrimPrefix(raw, "<!--"), "-->")
			raw = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(raw), "//<![CDATA["), "//]]>")

			var v any
			if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &v); err == nil {
				collect(v)
			}
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)

	return found
}

func isJSONLD(n *nethtml.Node) bool {
	for _, a := range n.Attr {
		if a.Key == "type" && strings.Contains(strings.ToLower(a.Val), "ld+json") {
			return true
		}
	}
	return false
}

func typesOf(obj map[string]any) map[string]bool {
	types := map[string]bool{}
	add := func(v any) {
		if s, ok := v.(string); ok {
			s = strings.ToLower(schemaValue(s))
			types[s] = true
		}
	}

	switch v := obj["@type"].(type) {
	case []any:
		for _, item := range v {
			add(item)
		}
	default:
		add(v)
	}

	return types
}

func hasTypeSuffix(types map[string]bool, suffix string) bool {
	for t := range types {
		if strings.HasSuffix(t, suffix) {
			return true
		}
	}
	return false
}

func productFact(obj map[string]any) string {
	name := text(obj["name"])
	if name == "" {
		return ""
	}

	parts := []string{"Produk: " + name}
	if brand := text(obj["brand"]); brand != "" && !strings.Contains(strings.ToLower(name), strings.ToLower(brand)) {
		parts = append(parts, "Merek: "+brand)
	}

	if offer := firstObject(obj["offers"]); offer != nil {
		currency := text(offer["priceCurrency"])
		price := text(offer["price"])
		if price == "" {
			if spec := firstObject(offer["priceSpecification"]); spec != nil {
				price = text(spec["price"])
				currency = firstNonEmpty(currency, text(spec["priceCurrency"]))
			}
		}

		switch low, high := text(offer["lowPrice"]), text(offer["highPrice"]); {
		case price != "":
			parts = append(parts, "Harga: "+strings.TrimSpace(price+" "+currency))
		case low != "" && high != "" && low != high:
			parts = append(parts, "Harga: "+strings.TrimSpace(low+" - "+high+" "+currency))
		case low != "":
			parts = append(parts, "Harga: "+strings.TrimSpace(low+" "+currency))
		}

		if availability := schemaValue(text(offer["availability"])); availability != "" {
			parts = append(parts, "Ketersediaan: "+availability)
		}
		if seller := text(offer["seller"]); seller != "" {
			parts = append(parts, "Penjual: "+seller)
		}
	}

	if rating := firstObject(obj["aggregateRating"]); rating != nil {
		if value := text(rating["ratingValue"]); value != "" {
			line := "Rating: " + value
			if count := firstNonEmpty(text(rating["reviewCount"]), text(rating["ratingCount"])); count != "" {
				line += " (" + count + " ulasan)"
			}
			parts = append(parts, line)
		}
	}

	return strings.Join(parts, " | ")
}

func eventFact(obj map[string]any) string {
	name := text(obj["name"])
	if name == "" {
		return ""
	}

	parts := []string{"Acara: " + name}
	if start := text(obj["startDate"]); start != "" {
		parts = append(parts, "Mulai: "+start)
	}
	if end := text(obj["endDate"]); end != "" {
		parts = append(parts, "Selesai: "+end)
	}
	if location := firstObject(obj["location"]); location != nil {
		place := strings.Trim(strings.Join(nonEmpty(text(location["name"]), address(location["address"])), ", "), ", ")
		if place != "" {
			parts = append(parts, "Lokasi: "+place)
		}
	} else if location := text(obj["location"]); location != "" {
		parts = append(parts, "Lokasi: "+location)
	}
	if status := schemaValue(text(obj["eventStatus"])); status != "" && status != "EventScheduled" {
		parts = append(parts, "Status: "+status)
	}

	return strings.Join(parts, " | ")
}

func placeFact(obj map[string]any) string {
	parts := nonEmpty(text(obj["name"]))
	if addr := address(obj["address"]); addr != "" {
		parts = append(parts, "Alamat: "+addr)
	}
	if phone := text(obj["telephone"]); phone != "" {
		parts = append(parts, "Telepon: "+phone)
	}
	if hours := openingHours(obj); hours != "" {
		parts = append(parts, "Jam buka: "+hours)
	}

	if len(parts) < 2 {
		return ""
	}
	return "Tempat: " + strings.Join(parts, " | ")
}

func openingHours(obj map[string]any) string {
	var hours []string

	switch v := obj["openingHours"].(type) {
	case string:
		hours = append(hours, cleanText(v))
	case []any:
		for _, item := range v {
			hours = append(hours, text(item))
		}
	}

	for _, spec := range objects(obj["openingHoursSpecification"]) {
		var days []string
		switch d := spec["dayOfWeek"].(type) {
		case string:
			days = append(days, schemaValue(d))
		case []any:
			for _, day := range d {
				days = append(days, schemaValue(text(day)))
			}
		}

		opens, closes := text(spec["opens"]), text(spec["closes"])
		if opens == "" && closes == "" {
			continue
		}
		hours = append(hours, strings.TrimSpace(strings.Join(days, ",")+" "+opens+"-"+closes))
	}

	return strings.Join(nonEmpty(hours...), "; ")
}

func faqBlocks(obj map[string]any) []block {
	var blocks []block
	for _, q := range objects(obj["mainEntity"]) {
		question := text(q["name"])
		answer := text(firstObject(q["acceptedAnswer"])["text"])
		if question == "" || answer == "" {
			continue
		}
		blocks = append(blocks, block{kind: blockText, text: "T: " + question}, block{kind: blockText, text: "J: " + answer})
	}

	if len(blocks) == 0 {
		return nil
	}
	return append([]block{{kind: blockHeading, text: "FAQ"}}, blocks...)
}

func recipeBlocks(obj map[string]any) []block {
	name := text(obj["name"])
	blocks := []block{{kind: blockHeading, text: strings.TrimSpace("Resep " + name)}}

	if info := strings.Join(nonEmpty(
		prefixed("Porsi: ", text(obj["recipeYield"])),
		prefixed("Waktu total: ", text(obj["totalTime"])),
	), " | "); info != "" {
		blocks = append(blocks, block{kind: blockText, text: info})
	}

	if ingredients := nonEmpty(texts(obj["recipeIngredient"])...); len(ingredients) > 0 {
		blocks = append(blocks, block{kind: blockHeading, text: "Bahan"})
		for _, line := range ingredients {
			blocks = append(blocks, block{kind: blockItem, text: line})
		}
	}

	var steps []string
	var collect func(any)
	collect = func(v any) {
		switch v := v.(type) {
		case string:
			steps = append(steps, cleanText(stripTags(v)))
		case []any:
			for _, item := range v {
				collect(item)
			}
		case map[string]any:
			if list, ok := v["itemListElement"]; ok {
				collect(list)
				return
			}
			steps = append(steps, text(v["text"]))
		}
	}
	collect(obj["recipeInstructions"])

	if steps = nonEmpty(steps...); len(steps) > 0 {
		blocks = append(blocks, block{kind: blockHeading, text: "Langkah"})
		for _, step := range steps {
			blocks = append(blocks, block{kind: blockItem, text: step})
		}
	}

	if len(blocks) == 1 {
		return nil
	}
	return blocks
}

func address(v any) string {
	switch v := v.(type) {
	case string:
		return cleanText(v)
	case map[string]any:
		return strings.Join(nonEmpty(
			text(v["streetAddress"]), text(v["addressLocality"]),
			text(v["addressRegion"]), text(v["postalCode"]),
		), ", ")
	case []any:
		if len(v) > 0 {
			return address(v[0])
		}
	}
	return ""
}

// text renders a JSON-LD value as one line of plain text: a string without
// markup, a number as written, an object by its name.
func text(v any) string {
	switch v := v.(type) {
	case string:
		return cleanText(stripTags(v))
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case map[string]any:
		return firstNonEmpty(text(v["name"]), text(v["@value"]))
	case []any:
		for _, item := range v {
			if s := text(item); s != "" {
				return s
			}
		}
	}
	return ""
}

func texts(v any) []string {
	switch v := v.(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			out = append(out, text(item))
		}
		return out
	default:
		return []string{text(v)}
	}
}

func firstObject(v any) map[string]any {
	switch v := v.(type) {
	case map[string]any:
		return v
	case []any:
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				return m
			}
		}
	}
	return nil
}

func objects(v any) []map[string]any {
	switch v := v.(type) {
	case map[string]any:
		return []map[string]any{v}
	case []any:
		var out []map[string]any
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}

// schemaValue strips the schema.org prefix from an enumeration value:
// "https://schema.org/InStock" becomes "InStock".
func schemaValue(s string) string {
	for _, prefix := range []string{"https://schema.org/", "http://schema.org/", "schema:"} {
		s = strings.TrimPrefix(s, prefix)
	}
	return s
}

func stripTags(s string) string {
	return html.UnescapeString(tagPattern.ReplaceAllString(s, " "))
}

func textContent(n *nethtml.Node) string {
	var b strings.Builder
	var walk func(*nethtml.Node)
	walk = func(n *nethtml.Node) {
		if n.Type == nethtml.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

var dateLayouts = []string{
	time.RFC3339, "2006-01-02T15:04:05-0700", "2006-01-02T15:04:05.000-0700",
	"2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02T15:04", "2006-01-02",
	"2006/01/02 15:04:05", "2006/01/02", time.RFC1123, time.RFC1123Z,
}

// parseDate reads the date formats pages use for publication times. A date
// before the web existed or more than a day in the future is treated as
// garbage.
func parseDate(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}

	for _, layout := range dateLayouts {
		if t, err := time.Parse(layout, raw); err == nil {
			if t.Year() < 1995 || t.After(time.Now().Add(24*time.Hour)) {
				return time.Time{}
			}
			return t
		}
	}

	return time.Time{}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func nonEmpty(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

func prefixed(prefix, value string) string {
	if value == "" {
		return ""
	}
	return prefix + value
}

func cleanText(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
