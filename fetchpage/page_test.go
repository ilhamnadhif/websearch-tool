package fetchpage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html"
)

// serve starts a local server and lets the package reach it: the SSRF guard
// refuses loopback, so tests swap in a plain client. The cache is cleared
// before and after.
func serve(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(handler)
	saved := httpClient
	httpClient = &http.Client{Timeout: 5 * time.Second}
	resetCache()

	t.Cleanup(func() {
		srv.Close()
		httpClient = saved
		resetCache()
	})

	return srv
}

func htmlPage(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, body)
	}
}

func mustFetch(t *testing.T, rawURL string, opts Options) Result {
	t.Helper()

	result, err := FetchWithOptions(context.Background(), rawURL, opts)
	if err != nil {
		t.Fatalf("FetchWithOptions(%s): %v", rawURL, err)
	}
	return result
}

func render(t *testing.T, markup string) string {
	t.Helper()

	root, err := html.Parse(strings.NewReader(markup))
	if err != nil {
		t.Fatal(err)
	}
	return blocksText(renderBlocks(root))
}

func TestRenderKeepsStructure(t *testing.T) {
	got := render(t, `<body>
		<h2>Tarif listrik</h2>
		<p>Berlaku   mulai<br>1 Oktober.</p>
		<ul><li>900 VA: Rp 1.352</li><li>1.300 VA: Rp <b>1.444,70</b></li></ul>
		<table><thead><tr><th>Daya</th><th>Tarif</th><th></th></tr></thead>
		<tbody><tr><td>900 VA</td><td>1.352</td><td></td></tr><tr><td>2.200 VA</td><td>1.444,70</td></tr></tbody></table>
		<p hidden>disembunyikan</p><p style="display: none">juga</p><div aria-hidden="true">dekorasi</div>
		<script>var x = 1</script><button>Bagikan</button>
	</body>`)

	want := strings.Join([]string{
		"## Tarif listrik",
		"Berlaku mulai",
		"1 Oktober.",
		"- 900 VA: Rp 1.352",
		"- 1.300 VA: Rp 1.444,70",
		"Daya | Tarif",
		"900 VA | 1.352",
		"2.200 VA | 1.444,70",
	}, "\n")
	if got != want {
		t.Errorf("render =\n%s\nwant\n%s", got, want)
	}
}

func TestRenderReadsLayoutTablesAsText(t *testing.T) {
	got := render(t, `<body><table><tr><td><p>Satu kolom saja.</p><p>Paragraf kedua.</p></td></tr></table>
		<table><tr><td>Menu</td><td><table><tr><td>a</td><td>b</td></tr></table><p>Isi utama</p></td></tr></table></body>`)

	if strings.Contains(got, "Satu kolom saja. | ") || !strings.Contains(got, "Satu kolom saja.\nParagraf kedua.") {
		t.Errorf("one-column table rendered as rows:\n%s", got)
	}
	if !strings.Contains(got, "Isi utama") {
		t.Errorf("nested layout lost its text:\n%s", got)
	}
}

const longArticle = `<html><head><title>Harga BBM Oktober 2026 - Contoh News</title>
<meta property="article:published_time" content="2026-10-01T08:00:00+07:00">
<meta name="description" content="Daftar harga BBM terbaru."></head><body>
<nav>Beranda | Ekonomi | Otomotif</nav>
<article><h1>Harga BBM Oktober 2026</h1>
<p>` + "Pertamina kembali mengumumkan harga BBM untuk bulan ini. Penyesuaian dilakukan mengikuti harga minyak dunia dan kurs rupiah. " + `</p>
<p>` + "Kenaikan dan penurunan berlaku serentak di seluruh SPBU sejak pukul 00.00 waktu setempat, menurut keterangan resmi perusahaan. " + `</p>
<p>` + "Pengendara diimbau memeriksa harga di aplikasi sebelum mengisi bahan bakar agar tidak terkejut di SPBU. " + `</p>
</article>
<table><tr><th>Produk</th><th>Harga per liter</th></tr><tr><td>Pertalite</td><td>Rp 10.000</td></tr><tr><td>Pertamax</td><td>Rp 12.500</td></tr></table>
<footer>Hak cipta</footer></body></html>`

func TestFetchAddsTablesTheArticleLeftOut(t *testing.T) {
	srv := serve(t, htmlPage(longArticle))

	result := mustFetch(t, srv.URL+"/bbm", Options{})

	if result.Source != SourceArticle {
		t.Fatalf("source = %q, want article", result.Source)
	}
	if !strings.Contains(result.Content, "Produk | Harga per liter\nPertalite | Rp 10.000") {
		t.Errorf("price table missing from content:\n%s", result.Content)
	}
	if strings.Contains(result.Content, "Hak cipta") || strings.Contains(result.Content, "Beranda | Ekonomi") {
		t.Errorf("navigation chrome leaked into content:\n%s", result.Content)
	}
	if want := time.Date(2026, 10, 1, 1, 0, 0, 0, time.UTC); !result.Published.Equal(want) {
		t.Errorf("published = %v, want %v", result.Published, want)
	}
}

func TestFetchStatesStructuredFactsFirst(t *testing.T) {
	page := `<html><head><title>iPhone 17 128GB</title>
	<script type="application/ld+json">{"@context":"https://schema.org","@graph":[
		{"@type":"Product","name":"iPhone 17 128GB","brand":{"@type":"Brand","name":"Apple"},
		 "offers":{"@type":"Offer","price":16999000,"priceCurrency":"IDR","availability":"https://schema.org/InStock"},
		 "aggregateRating":{"ratingValue":4.8,"reviewCount":1234}},
		{"@type":"GovernmentOffice","name":"Samsat Jakarta Selatan","address":{"streetAddress":"Jl. Gatot Subroto","addressLocality":"Jakarta Selatan"},
		 "openingHoursSpecification":[{"dayOfWeek":["Monday","Friday"],"opens":"08:00","closes":"15:00"}]},
		{"@type":"FAQPage","mainEntity":[{"@type":"Question","name":"Berapa garansinya?","acceptedAnswer":{"@type":"Answer","text":"<p>Satu tahun resmi.</p>"}}]}
	]}</script>
	<script type="application/ld+json">{ not json }</script>
	</head><body><p>` + strings.Repeat("Deskripsi produk yang panjang. ", 12) + `</p></body></html>`

	srv := serve(t, htmlPage(page))
	result := mustFetch(t, srv.URL+"/iphone", Options{})

	lines := strings.Split(result.Content, "\n")
	if lines[0] != "Produk: iPhone 17 128GB | Merek: Apple | Harga: 16999000 IDR | Ketersediaan: InStock | Rating: 4.8 (1234 ulasan)" {
		t.Errorf("first line = %q", lines[0])
	}
	if lines[1] != "Tempat: Samsat Jakarta Selatan | Alamat: Jl. Gatot Subroto, Jakarta Selatan | Jam buka: Monday,Friday 08:00-15:00" {
		t.Errorf("second line = %q", lines[1])
	}
	if !strings.Contains(result.Content, "## FAQ\nT: Berapa garansinya?\nJ: Satu tahun resmi.") {
		t.Errorf("FAQ missing:\n%s", result.Content)
	}
}

func TestFetchReadsARecipe(t *testing.T) {
	page := `<html><head><script type="application/ld+json">{"@type":"Recipe","name":"Rendang Daging",
		"recipeYield":"6 porsi","totalTime":"PT4H","recipeIngredient":["1 kg daging sapi","2 liter santan"],
		"recipeInstructions":[{"@type":"HowToSection","itemListElement":[{"@type":"HowToStep","text":"Haluskan bumbu."},{"@type":"HowToStep","text":"Masak hingga kering."}]}]}</script>
		</head><body><p>` + strings.Repeat("Cerita panjang tentang rendang. ", 10) + `</p></body></html>`

	srv := serve(t, htmlPage(page))
	result := mustFetch(t, srv.URL+"/rendang", Options{})

	for _, want := range []string{"## Resep Rendang Daging", "Porsi: 6 porsi | Waktu total: PT4H", "## Bahan\n- 1 kg daging sapi\n- 2 liter santan", "## Langkah\n- Haluskan bumbu.\n- Masak hingga kering."} {
		if !strings.Contains(result.Content, want) {
			t.Errorf("content lacks %q:\n%s", want, result.Content)
		}
	}
}

func TestFetchFocusPicksTheAnsweringPassage(t *testing.T) {
	var b strings.Builder
	b.WriteString(`<html><head><title>Kurs</title></head><body><article><h1>Kurs Transaksi</h1><p>Data kurs resmi diperbarui setiap hari kerja.</p>`)
	for i := range 12 {
		fmt.Fprintf(&b, "<h2>Bagian %d</h2><p>%s</p>", i, strings.Repeat(fmt.Sprintf("Paragraf pengisi nomor %d tentang hal lain. ", i), 12))
	}
	b.WriteString(`<h2>Tabel kurs</h2><table><tr><th>Mata Uang</th><th>Kurs Jual</th><th>Kurs Beli</th></tr>`)
	for _, code := range []string{"AED", "AUD", "CNY", "EUR", "GBP", "JPY", "MYR", "SGD", "THB", "USD"} {
		fmt.Fprintf(&b, "<tr><td>%s</td><td>1.000</td><td>990</td></tr>", code)
	}
	b.WriteString(`</table></article></body></html>`)

	srv := serve(t, htmlPage(b.String()))

	plain := mustFetch(t, srv.URL+"/kurs", Options{MaxChars: 1500})
	if strings.Contains(plain.Content, "USD") {
		t.Fatal("premise broken: USD fits in the opening without a focus")
	}

	// The focus names neither the heading nor the column names, so they can
	// only arrive as the context of the rows chosen mid-table.
	focused := mustFetch(t, srv.URL+"/kurs", Options{Focus: "USD", MaxChars: 1500})
	lines := strings.Split(focused.Content, "\n")
	usd := slices.Index(lines, "USD | 1.000 | 990")
	if usd < 0 {
		t.Fatalf("focus missed the USD row:\n%s", focused.Content)
	}
	first := usd
	for first > 0 && strings.Count(lines[first-1], " | ") == 2 && !strings.HasPrefix(lines[first-1], "Mata Uang") {
		first--
	}
	if first < 2 || lines[first-1] != "Mata Uang | Kurs Jual | Kurs Beli" || lines[first-2] != "## Tabel kurs" {
		t.Errorf("the chosen rows came without their heading and column names:\n%s", focused.Content)
	}
	if !strings.Contains(focused.Content, gapMarker) {
		t.Errorf("no marker where text was left out:\n%s", focused.Content)
	}
	if n := utf8.RuneCountInString(focused.Content); n > 1500 {
		t.Errorf("content is %d characters, over MaxChars", n)
	}
}

func TestFetchRetriesHonestlyAfterARefusal(t *testing.T) {
	var agents []string
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		agents = append(agents, r.UserAgent())
		if r.UserAgent() != HonestUserAgent {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		htmlPage(longArticle)(w, r)
	})

	result := mustFetch(t, srv.URL+"/wiki", Options{})
	if result.Title == "" || len(agents) != 2 || agents[1] != HonestUserAgent {
		t.Fatalf("title=%q agents=%v", result.Title, agents)
	}
}

func TestFetchReportsABlockAsErrBlocked(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, `<html><body><script>challenge()</script></body></html>`)
	})

	_, err := FetchWithOptions(context.Background(), srv.URL+"/kompas", Options{})
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("err = %v, want ErrBlocked", err)
	}
}

func TestFetchJavaScriptShell(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/described" {
			fmt.Fprint(w, `<html><head><meta property="og:title" content="iBox"><meta name="description" content="Toko resmi Apple di Indonesia."></head><body><div id="root">Loading...</div><noscript>Enable JavaScript</noscript></body></html>`)
			return
		}
		fmt.Fprint(w, `<html><body><div id="root"></div><script src="app.js"></script></body></html>`)
	})

	result := mustFetch(t, srv.URL+"/described", Options{})
	if result.Source != SourceSummary || result.Content != "Toko resmi Apple di Indonesia." || result.Title != "iBox" {
		t.Errorf("result = %+v, want the description as a summary", result)
	}

	if _, err := FetchWithOptions(context.Background(), srv.URL+"/bare", Options{}); !errors.Is(err, ErrNoContent) {
		t.Errorf("err = %v, want ErrNoContent", err)
	}
}

func TestFetchDecodesLegacyCharsets(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=windows-1252")
		body := "<html><body><p>" + strings.Repeat("Caf\xe9 di Bandung buka sampai malam. ", 10) + "</p></body></html>"
		fmt.Fprint(w, body)
	})

	result := mustFetch(t, srv.URL+"/cafe", Options{})
	if !strings.Contains(result.Content, "Café di Bandung") || !utf8.ValidString(result.Content) {
		t.Errorf("content not decoded to UTF-8: %q", result.Content[:60])
	}
}

func TestFetchReadsPDFAndText(t *testing.T) {
	saved := PDFToText
	t.Cleanup(func() { PDFToText = saved })

	var got []byte
	PDFToText = func(_ context.Context, pdf []byte) (string, error) {
		got = pdf
		return "Peraturan Menteri Keuangan\n\nPasal 1\nTarif bea\nmasuk ditetapkan.\n", nil
	}

	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/pmk.pdf":
			w.Header().Set("Content-Type", "application/pdf")
			fmt.Fprint(w, "%PDF-1.4 isi")
		case "/catatan.txt":
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprint(w, "Catatan rapat\n\nHasil keputusan ada di sini.")
		default:
			w.Header().Set("Content-Type", "application/zip")
			fmt.Fprint(w, "PK")
		}
	})

	pdf := mustFetch(t, srv.URL+"/pmk.pdf", Options{})
	if pdf.Source != SourcePDF || pdf.Title != "Peraturan Menteri Keuangan" || !strings.Contains(pdf.Content, "Pasal 1 Tarif bea masuk ditetapkan.") {
		t.Errorf("pdf result = %+v", pdf)
	}
	if !bytes.HasPrefix(got, []byte("%PDF")) {
		t.Errorf("converter got %q", got)
	}

	text := mustFetch(t, srv.URL+"/catatan.txt", Options{})
	if text.Source != SourceText || !strings.Contains(text.Content, "Hasil keputusan") {
		t.Errorf("text result = %+v", text)
	}

	if _, err := FetchWithOptions(context.Background(), srv.URL+"/arsip.zip", Options{}); err == nil || !strings.Contains(err.Error(), "konten bukan HTML") {
		t.Errorf("zip err = %v", err)
	}

	PDFToText = nil
	resetCache()
	if _, err := FetchWithOptions(context.Background(), srv.URL+"/pmk.pdf", Options{}); !errors.Is(err, ErrPDFUnsupported) {
		t.Errorf("err = %v, want ErrPDFUnsupported without a converter", err)
	}
}

func TestPopplerReadsARealPDF(t *testing.T) {
	if _, err := exec.LookPath("pdftotext"); err != nil {
		t.Skip("pdftotext not installed")
	}

	text, err := popplerPDFToText(context.Background(), minimalPDF("Harga emas hari ini"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "Harga emas hari ini") {
		t.Errorf("pdftotext output = %q", text)
	}
}

// minimalPDF builds a one-page PDF that shows text, with a correct xref.
func minimalPDF(text string) []byte {
	stream := "BT /F1 12 Tf 72 720 Td (" + text + ") Tj ET"
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}

	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects))
	for i, obj := range objects {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, obj)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return b.Bytes()
}

func TestFetchReadsMSNThroughItsAPI(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/detail/id-id/AA2dDPAY" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"title":"5 Berita Terpopuler","publishedDateTime":"2026-10-05T23:53:00Z","provider":{"name":"JPNN.com"},
			"body":"<img src=\"x\"/><p><span>jpnn.com</span>, JAKARTA - Selamat pagi.</p><p><strong>1. Prabowo</strong></p>"}`)
	})

	saved := msnDetailEndpoint
	msnDetailEndpoint = srv.URL + "/detail"
	t.Cleanup(func() { msnDetailEndpoint = saved })

	result := mustFetch(t, "https://www.msn.com/id-id/berita/other/5-berita-terpopuler/ar-AA2dDPAY?ocid=x", Options{})
	if result.Source != SourceMSN || result.Title != "5 Berita Terpopuler" {
		t.Fatalf("result = %+v", result)
	}
	if result.Content != "Sumber asli: JPNN.com\njpnn.com, JAKARTA - Selamat pagi.\n1. Prabowo" {
		t.Errorf("content = %q", result.Content)
	}
	if want := time.Date(2026, 10, 5, 23, 53, 0, 0, time.UTC); !result.Published.Equal(want) {
		t.Errorf("published = %v", result.Published)
	}
}

func TestMSNArticleRecognition(t *testing.T) {
	cases := map[string][2]string{
		"https://www.msn.com/id-id/berita/other/judul/ar-AA2dDPAY":  {"id-id", "AA2dDPAY"},
		"https://www.msn.com/en-us/news/world/title/ar-BB1x?ocid=1": {"en-us", "BB1x"},
		"https://msn.com/ar-CC9":                                    {"en-us", "CC9"},
	}
	for raw, want := range cases {
		u, _ := url.Parse(raw)
		locale, id, ok := msnArticle(u)
		if !ok || locale != want[0] || id != want[1] {
			t.Errorf("msnArticle(%s) = %q, %q, %v", raw, locale, id, ok)
		}
	}

	for _, raw := range []string{"https://www.msn.com/id-id/berita", "https://notmsn.com/ar-AA1", "https://www.msn.com/id-id/video/vi-AA1"} {
		u, _ := url.Parse(raw)
		if _, _, ok := msnArticle(u); ok {
			t.Errorf("msnArticle(%s) matched", raw)
		}
	}
}

func TestFetchReadsWikipediaThroughItsAPI(t *testing.T) {
	var agent, titles atomic.Value
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		agent.Store(r.UserAgent())
		titles.Store(r.URL.Query().Get("titles"))
		fmt.Fprint(w, `{"query":{"pages":[{"title":"Menteri Keuangan Indonesia","extract":"Menteri Keuangan adalah kepala kementerian.\n\n\n== Sejarah ==\nDibentuk tahun 1945.\n\n== Referensi ==\nCatatan kaki panjang.\n\n== Pranala luar ==\nSitus resmi."}]}}`)
	})

	saved := wikipediaAPI
	wikipediaAPI = func(host string) string { return srv.URL + "/" + host + "/w/api.php" }
	t.Cleanup(func() { wikipediaAPI = saved })

	result := mustFetch(t, "https://id.m.wikipedia.org/wiki/Menteri_Keuangan_Indonesia", Options{})
	if result.Source != SourceWikipedia || result.Title != "Menteri Keuangan Indonesia" {
		t.Fatalf("result = %+v", result)
	}
	if result.Content != "Menteri Keuangan adalah kepala kementerian.\n## Sejarah\nDibentuk tahun 1945." {
		t.Errorf("content = %q", result.Content)
	}
	if agent.Load() != HonestUserAgent || titles.Load() != "Menteri Keuangan Indonesia" {
		t.Errorf("agent=%v titles=%v", agent.Load(), titles.Load())
	}
}

func TestFetchCachesPagesNotFailures(t *testing.T) {
	var hits atomic.Int32
	failing := atomic.Bool{}
	failing.Store(true)

	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if failing.Load() {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		htmlPage(longArticle)(w, r)
	})

	if _, err := FetchWithOptions(context.Background(), srv.URL+"/a", Options{}); err == nil {
		t.Fatal("want the 500 reported")
	}
	failing.Store(false)

	first := mustFetch(t, srv.URL+"/a", Options{})
	second := mustFetch(t, srv.URL+"/a#bagian", Options{Focus: "pertamax", MaxChars: 300})
	if hits.Load() != 2 {
		t.Errorf("server hit %d times, want 2: the failure must not be cached, the page must be", hits.Load())
	}
	if first.Content == second.Content || utf8.RuneCountInString(second.Content) > 300 {
		t.Errorf("cached page not reshaped by the second call's options")
	}
}

func TestFetchRejectsOversizedBodies(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write(bytes.Repeat([]byte("a"), maxBodyBytes+10))
	})

	if _, err := FetchWithOptions(context.Background(), srv.URL+"/besar", Options{}); err == nil || !strings.Contains(err.Error(), "terlalu besar") {
		t.Errorf("err = %v", err)
	}
}

func TestFetchManyKeepsOrderAndCaps(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rusak" {
			http.NotFound(w, r)
			return
		}
		htmlPage(`<html><head><title>`+r.URL.Path+`</title></head><body><p>`+strings.Repeat("Isi halaman yang cukup panjang. ", 10)+`</p></body></html>`)(w, r)
	})

	urls := []string{srv.URL + "/1", srv.URL + "/rusak", srv.URL + "/3", srv.URL + "/4", srv.URL + "/5", srv.URL + "/6"}
	results := FetchMany(context.Background(), urls)

	if len(results) != maxURLsPerCall {
		t.Fatalf("len = %d, want %d", len(results), maxURLsPerCall)
	}
	if results[0].Title != "/1" || results[2].Title != "/3" || results[1].Error == "" || results[1].URL != srv.URL+"/rusak" {
		t.Errorf("results = %+v", results)
	}
}

func TestFocusFallsBackToTheOpeningWhenNoPassageFits(t *testing.T) {
	blocks := []block{{kind: blockRow, text: strings.Repeat("USD 1.000 ", 200), table: 1, head: true}}

	got := selectText(blocks, "USD", 300)
	if got == gapMarker || utf8.RuneCountInString(got) > 300 || !strings.HasPrefix(got, "USD 1.000") {
		t.Errorf("selectText = %q, want the opening cut to the budget", got)
	}
}

func TestRenderSplitsVeryLongParagraphs(t *testing.T) {
	var paragraph strings.Builder
	for i := range 40 {
		fmt.Fprintf(&paragraph, "Kalimat ke-%d tentang tarif listrik yang terus berlanjut. ", i)
	}
	got := render(t, "<body><p>"+paragraph.String()+"</p></body>")

	lines := strings.Split(got, "\n")
	if len(lines) < 3 {
		t.Fatalf("paragraph of %d characters stayed in %d block(s)", paragraph.Len(), len(lines))
	}
	for _, line := range lines {
		if len(line) > passageChars || !strings.HasSuffix(line, ".") {
			t.Errorf("block of %d characters, ending %q", len(line), line[max(0, len(line)-10):])
		}
	}
}

func TestFetchReadsProductsListedInAnItemList(t *testing.T) {
	page := `<html><head><script type="application/ld+json">{"@type":"ItemList","itemListElement":[
		{"@type":"ListItem","position":1,"item":{"@type":"Product","name":"iPhone 17","offers":{"@type":"AggregateOffer","lowPrice":"16999000","highPrice":"20999000","priceCurrency":"IDR"}}},
		{"@type":"ListItem","position":2,"item":{"@type":"Product","name":"iPhone Air","offers":{"@type":"Offer","price":"19999000","priceCurrency":"IDR"}}}
	]}</script></head><body><p>` + strings.Repeat("Daftar produk. ", 20) + `</p></body></html>`

	srv := serve(t, htmlPage(page))
	result := mustFetch(t, srv.URL+"/daftar", Options{})

	for _, want := range []string{"Produk: iPhone 17 | Harga: 16999000 - 20999000 IDR", "Produk: iPhone Air | Harga: 19999000 IDR"} {
		if !strings.Contains(result.Content, want) {
			t.Errorf("content lacks %q:\n%s", want, result.Content)
		}
	}
}

func TestFocusFillsLeftoverRoomWithThePageInOrder(t *testing.T) {
	var blocks []block
	for i := range 30 {
		text := fmt.Sprintf("Paragraf %d tentang sejarah mata uang dan perkembangannya dari masa ke masa di berbagai daerah. ", i)
		text += strings.Repeat("Isi tambahan. ", 15)
		if i == 25 {
			text = "Kurs jual USD hari ini Rp16.512 menurut Bank Indonesia."
		}
		blocks = append(blocks, block{kind: blockText, text: text})
	}

	got := selectText(blocks, "kurs jual USD", 2000)

	if !strings.Contains(got, "Kurs jual USD hari ini Rp16.512") {
		t.Fatalf("the matching passage is missing:\n%s", got)
	}
	if !strings.HasPrefix(got, "Paragraf 0 ") {
		t.Errorf("leftover room was not filled from the page's opening:\n%s", got)
	}
	if n := utf8.RuneCountInString(got); n < 1500 || n > 2000 {
		t.Errorf("content is %d characters, want the 2000 budget mostly used", n)
	}
	if strings.Index(got, "Paragraf 0 ") > strings.Index(got, "Kurs jual USD") {
		t.Error("passages are out of page order")
	}
}

// Found in review before v1.3.0 was tagged: each of these returned something
// worse than v1.2.0 did.

func TestFetchKeepsUndeclaredUTF8(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		// Plain ASCII fills the first kilobyte, where charset sniffing looks.
		fmt.Fprint(w, "<html><body><p>"+strings.Repeat("Menu kedai kopi di Bandung. ", 60)+"</p><p>Kopi susu café — Rp25.000.</p></body></html>")
	})

	result := mustFetch(t, srv.URL+"/kopi", Options{})
	if !strings.Contains(result.Content, "café — Rp25.000") {
		t.Errorf("UTF-8 page decoded as a legacy charset: %q", result.Content[len(result.Content)-60:])
	}
}

func TestFetchReturnsAShortRealPage(t *testing.T) {
	srv := serve(t, htmlPage(`<html><body><p>Kurs jual USD hari ini: Rp 16.420 (sumber: Bank Indonesia).</p></body></html>`))

	result := mustFetch(t, srv.URL+"/singkat", Options{})
	if result.Source != SourcePage || result.Content != "Kurs jual USD hari ini: Rp 16.420 (sumber: Bank Indonesia)." {
		t.Errorf("result = %+v, want the short page itself", result)
	}
}

func TestFocusNeverShrinksToTheOpeningAlone(t *testing.T) {
	// One list item longer than the whole budget, the answer at its end.
	var item strings.Builder
	for i := range 150 {
		fmt.Fprintf(&item, "Kalimat ke-%d tentang mata uang lain. ", i)
	}
	item.WriteString("Kurs USD hari ini Rp16.420. ")
	if item.Len() <= 4000 {
		t.Fatalf("premise broken: the item is %d bytes, within the budget", item.Len())
	}

	root, err := html.Parse(strings.NewReader(`<body><h2>Kurs</h2><p>Data kurs resmi.</p><h2>Tabel</h2><ul><li>` + item.String() + `</li></ul></body>`))
	if err != nil {
		t.Fatal(err)
	}

	got := selectText(renderBlocks(root), "kurs USD", 4000)
	if !strings.Contains(got, "Kurs USD hari ini Rp16.420") {
		t.Errorf("the matching text is missing (%d characters):\n%s", utf8.RuneCountInString(got), got)
	}
	if utf8.RuneCountInString(got) < 1000 {
		t.Errorf("focus returned %d characters, less than the plain opening would", utf8.RuneCountInString(got))
	}
}

func TestFetchRejectsUnsupportedTypesBeforeDownloading(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/unduh" {
			w.Header().Set("Content-Type", "application/octet-stream")
			fmt.Fprint(w, "%PDF-1.4 isi")
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.(http.Flusher).Flush()
		chunk := bytes.Repeat([]byte("v"), 64<<10)
		for range 40 {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	})

	start := time.Now()
	_, err := FetchWithOptions(context.Background(), srv.URL+"/video.mp4", Options{})
	if err == nil || !strings.Contains(err.Error(), "konten bukan HTML") {
		t.Fatalf("err = %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("rejecting a video took %v: the body was downloaded first", elapsed)
	}

	saved := PDFToText
	t.Cleanup(func() { PDFToText = saved })
	PDFToText = func(context.Context, []byte) (string, error) { return "Dokumen unduhan\n\nIsi PDF.", nil }

	result := mustFetch(t, srv.URL+"/unduh", Options{})
	if result.Source != SourcePDF || !strings.Contains(result.Content, "Isi PDF.") {
		t.Errorf("a PDF served as octet-stream was not read: %+v", result)
	}
}

func TestTextDocumentsAreCapped(t *testing.T) {
	text := strings.Repeat("Baris teks panjang dalam dokumen besar.\n\n", 120_000)

	blocks := textBlocks(text)
	if n := blocksLen(blocks); n > maxRenderedChars+2*passageChars {
		t.Errorf("text document kept %d characters, want at most about %d", n, maxRenderedChars)
	}

	wiki := wikiTextBlocks(strings.Repeat("Paragraf ensiklopedia yang panjang sekali.\n", 120_000))
	if n := blocksLen(wiki); n > maxRenderedChars+2*passageChars {
		t.Errorf("wikipedia extract kept %d characters", n)
	}
}

func TestFetchManyWithFocusIsSafeConcurrently(t *testing.T) {
	var b strings.Builder
	for i := range 40 {
		fmt.Fprintf(&b, "<h2>Bagian %d</h2><p>%s</p>", i, strings.Repeat(fmt.Sprintf("Isi bagian %d tentang harga dan kurs. ", i), 10))
	}
	srv := serve(t, htmlPage("<html><body><article>"+b.String()+"</article></body></html>"))

	urls := []string{srv.URL + "/a", srv.URL + "/b", srv.URL + "/c", srv.URL + "/d", srv.URL + "/e"}
	for _, r := range FetchManyWithOptions(context.Background(), urls, Options{Focus: "kurs bagian 33", MaxChars: 1200}) {
		if r.Error != "" || !strings.Contains(r.Content, "Bagian 33") {
			t.Errorf("%s: error=%q content lacks the focused section", r.URL, r.Error)
		}
	}
}
