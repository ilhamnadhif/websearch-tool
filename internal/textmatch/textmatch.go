// Package textmatch holds the word matching that websearch (is this result
// about the query?) and fetchpage (which part of this page answers the
// question?) share: folding text to plain lower-case words, the words that
// carry no meaning, and a match that tolerates Indonesian suffixes.
package textmatch

import (
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// Fold lower-cases s, strips diacritics, and turns everything that is not a
// letter or digit into single spaces, so "Kurs-Rupiah, Hari Ini!" becomes
// "kurs rupiah hari ini". It is safe for concurrent use.
func Fold(s string) string {
	// A transform.Chain keeps buffers between calls, so one shared chain
	// panics under concurrent use: every call builds its own.
	stripMarks := transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	if folded, _, err := transform.String(stripMarks, s); err == nil {
		s = folded
	}

	var b strings.Builder
	space := true
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			space = false
			continue
		}
		if !space {
			b.WriteByte(' ')
			space = true
		}
	}

	return strings.TrimSpace(b.String())
}

// Words returns the folded words of s.
func Words(s string) []string {
	return strings.Fields(Fold(s))
}

var stopwords = set(
	// Indonesian function and question words, informal spellings included.
	"yang", "dan", "di", "ke", "dari", "untuk", "utk", "dengan", "dgn", "pada",
	"adalah", "ialah", "ini", "itu", "atau", "juga", "dalam", "akan", "tidak",
	"tak", "ada", "apa", "apakah", "siapa", "kapan", "dimana", "mana",
	"bagaimana", "gimana", "berapa", "brp", "mengapa", "kenapa", "bisa", "dapat",
	"sudah", "udah", "belum", "saja", "aja", "gak", "ga", "nggak", "enggak",
	"yg", "tentang", "oleh", "sebagai", "karena", "jika", "kalau", "kalo", "agar",
	"supaya", "para", "pun", "lah", "kah", "dong", "sih", "nya", "si", "sang",
	"per", "saya", "aku", "kamu", "anda", "kita", "kami", "mereka", "dia", "ia",
	"tolong", "mohon", "coba", "cari", "carikan", "info", "informasi",
	// Time words: the pages that answer "... hari ini" rarely say so.
	"hari", "sekarang", "terbaru", "terkini", "besok", "kemarin", "lusa",
	"nanti", "tadi", "saat", "kini", "update",
	// English.
	"the", "a", "an", "of", "in", "on", "for", "to", "and", "or", "is", "are",
	"was", "were", "be", "what", "who", "when", "where", "how", "why", "which",
	"with", "by", "at", "from", "about", "today", "now", "latest", "near", "me",
	"my", "vs", "versus",
	// Address noise.
	"www", "com", "http", "https", "site",
)

// generic words frame a question ("harga ...", "jadwal ...", "... jakarta")
// without saying what it is about, and they appear on countless unrelated
// pages.
var generic = set(
	"harga", "jadwal", "cara", "syarat", "biaya", "daftar", "contoh", "arti",
	"pengertian", "lokasi", "alamat", "jam", "buka", "tutup", "promo", "diskon",
	"resep", "tips", "panduan", "tutorial", "kumpulan", "terlengkap",
	"terbaik", "termurah", "murah", "mahal", "gratis", "online", "resmi",
	"lengkap", "berita", "kabar", "review", "ulasan", "spesifikasi",
	"rekomendasi", "link", "download", "unduh", "baru", "kota", "kabupaten",
	"provinsi", "kecamatan", "indonesia", "jakarta", "tahun", "bulan", "minggu",
	"tanggal", "januari", "februari", "maret", "april", "mei", "juni", "juli",
	"agustus", "september", "oktober", "november", "desember",
	"price", "best", "top", "cheap", "new", "free", "official", "news", "list",
)

func set(words ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(words))
	for _, w := range words {
		m[w] = struct{}{}
	}
	return m
}

// IsStopword reports whether the folded word w carries no meaning of its own.
func IsStopword(w string) bool {
	_, ok := stopwords[w]
	return ok
}

// IsGeneric reports whether the folded word w frames a question rather than
// naming its subject.
func IsGeneric(w string) bool {
	_, ok := generic[w]
	return ok
}

// IsLatin reports whether every letter of w is Latin. Word matching says
// little about text in other scripts.
func IsLatin(w string) bool {
	for _, r := range w {
		if unicode.IsLetter(r) && !unicode.Is(unicode.Latin, r) {
			return false
		}
	}
	return true
}

// IsNumber reports whether w is all digits.
func IsNumber(w string) bool {
	for _, r := range w {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return w != ""
}

// Text is a piece of text prepared for repeated term lookups.
type Text struct {
	words []string
	set   map[string]bool
}

// NewText folds s into words for Has.
func NewText(s string) Text {
	words := Words(s)

	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}

	return Text{words: words, set: m}
}

// Has reports whether the text mentions the folded term. Short terms
// (acronyms such as "krl" or "sim") must match a whole word, or "sim" would
// match "simak". Longer ones also match across suffixes in either direction:
// "harga" finds "harganya" and "perpanjangan" finds "perpanjang". A match
// never starts inside a word, so "menteri" is not found in "kementerian".
func (t Text) Has(term string) bool {
	if t.set[term] {
		return true
	}
	for _, w := range t.words {
		if matchWord(w, term) {
			return true
		}
	}
	return false
}

// Count reports how many words of the text match term, as Has decides.
func (t Text) Count(term string) int {
	n := 0
	for _, w := range t.words {
		if matchWord(w, term) {
			n++
		}
	}
	return n
}

// suffixes are the endings a four-letter term may carry: "emas" matches
// "emasnya", but "tari" (dance) does not match "tarif" (rate).
var suffixes = set("nya", "an", "kan", "i", "lah", "kah", "pun", "ku", "mu", "s", "es")

func matchWord(w, term string) bool {
	if w == term {
		return true
	}
	if len(term) < 4 {
		return false
	}

	if strings.HasPrefix(w, term) {
		if len(term) >= 5 {
			return true
		}
		_, ok := suffixes[w[len(term):]]
		return ok
	}

	// The word is the term minus a suffix: "perpanjang" for
	// "perpanjangan". It must keep most of the term (80%, at least four
	// letters), so "per" never stands in for "perpanjangan".
	if len(term) >= 5 && strings.HasPrefix(term, w) {
		return len(w) >= max(4, (len(term)*4+4)/5)
	}

	return false
}
