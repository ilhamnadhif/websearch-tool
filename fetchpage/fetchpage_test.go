package fetchpage

import (
	"context"
	"net"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/PuerkitoBio/goquery"
)

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()

	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}

	return u
}

func TestIsBlockedIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1",
		"127.8.8.8",
		"::1",
		"10.1.2.3",
		"172.16.0.9",
		"172.31.255.254",
		"192.168.1.1",
		"169.254.169.254", // cloud metadata endpoint
		"fe80::1",
		"0.0.0.0",
		"224.0.0.1",
		"ff02::1",
	}

	for _, s := range blocked {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("fixture IP %q tidak valid", s)
		}
		if !isBlockedIP(ip) {
			t.Errorf("isBlockedIP(%s) = false, want true", s)
		}
	}

	allowed := []string{
		"8.8.8.8",
		"1.1.1.1",
		"142.250.4.113",
		"2606:4700::1111",
		"172.32.0.1", // just outside RFC1918 range
	}

	for _, s := range allowed {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("fixture IP %q tidak valid", s)
		}
		if isBlockedIP(ip) {
			t.Errorf("isBlockedIP(%s) = true, want false", s)
		}
	}
}

func TestSafeDialContextBlocksLoopback(t *testing.T) {
	_, err := safeDialContext(context.Background(), "tcp", "localhost:1234")
	if err == nil {
		t.Fatal("dial ke localhost lolos, harusnya diblokir")
	}
	if !strings.Contains(err.Error(), "diblokir") {
		t.Errorf("error tak terduga: %v", err)
	}
}

func TestTruncateContent(t *testing.T) {
	t.Run("pendek utuh", func(t *testing.T) {
		in := strings.Repeat("a", 100)
		if got := truncateContent(in); got != in {
			t.Error("konten pendek berubah")
		}
	})

	t.Run("tepat batas", func(t *testing.T) {
		in := strings.Repeat("é", maxContentChars)
		got := truncateContent(in)
		if utf8.RuneCountInString(got) != maxContentChars || !utf8.ValidString(got) {
			t.Errorf("batas eksak rusak: runes=%d valid=%v", utf8.RuneCountInString(got), utf8.ValidString(got))
		}
	})

	t.Run("ASCII terpotong", func(t *testing.T) {
		got := truncateContent(strings.Repeat("x", 9000))
		if len(got) != maxContentChars {
			t.Errorf("len = %d, want %d", len(got), maxContentChars)
		}
	})

	t.Run("multibyte tidak terpotong di tengah rune", func(t *testing.T) {
		got := truncateContent(strings.Repeat("é", 5000))
		if utf8.RuneCountInString(got) != maxContentChars {
			t.Errorf("runes = %d, want %d", utf8.RuneCountInString(got), maxContentChars)
		}
		if !utf8.ValidString(got) {
			t.Error("hasil bukan UTF-8 valid — rune terpotong di tengah")
		}
	})

	t.Run("emoji empat byte", func(t *testing.T) {
		got := truncateContent(strings.Repeat("\U0001F600", 5000))
		if utf8.RuneCountInString(got) != maxContentChars || !utf8.ValidString(got) {
			t.Errorf("emoji: runes=%d valid=%v", utf8.RuneCountInString(got), utf8.ValidString(got))
		}
	})
}

func TestExtractCleanTextStripsChrome(t *testing.T) {
	html := `<html><head><style>.x{color:red}</style></head><body>
		<nav>Menu Navigasi</nav>
		<script>alert('x')</script>
		<h1>Judul Produk</h1>
		<p>Konten utama yang   dipertahankan.</p>
		<footer>Hak cipta</footer>
	</body></html>`

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		t.Fatal(err)
	}

	text := extractCleanText(doc)

	for _, unwanted := range []string{"Navigasi", "alert", "Hak cipta", "color:red"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("hasil masih memuat %q: %q", unwanted, text)
		}
	}
	for _, wanted := range []string{"Judul Produk", "Konten utama yang dipertahankan."} {
		if !strings.Contains(text, wanted) {
			t.Errorf("hasil kehilangan %q: %q", wanted, text)
		}
	}
}

func TestExtractReadableArticleVersusSparsePage(t *testing.T) {
	paragraph := strings.Repeat("Kalimat artikel yang cukup panjang untuk lulus ambang keterbacaan. ", 10)
	article := `<html><head><title>Judul Artikel</title></head><body><article><p>` +
		paragraph + `</p></article></body></html>`

	title, content, ok := extractReadable([]byte(article), mustURL(t, "https://example.com/post"))
	if !ok {
		t.Fatal("artikel normal ditolak readability")
	}
	if title != "Judul Artikel" || !strings.Contains(content, "Kalimat artikel") {
		t.Errorf("title=%q content len=%d", title, len(content))
	}

	sparse := `<html><head><title>Kosong</title></head><body><p>terlalu pendek</p></body></html>`
	if _, _, ok := extractReadable([]byte(sparse), mustURL(t, "https://example.com/short")); ok {
		t.Error("halaman sparse diterima, harusnya fallback ke strip-tag")
	}
}

func TestFetchRejectsNonHTTPScheme(t *testing.T) {
	if _, _, err := Fetch(context.Background(), "ftp://example.com/file"); err == nil {
		t.Fatal("skema ftp lolos tanpa error")
	} else if !strings.Contains(err.Error(), "skema") {
		t.Errorf("error tak terduga: %v", err)
	}
}
