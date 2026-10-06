package websearch

import (
	"reflect"
	"testing"
)

func termWords(terms []term) []string {
	var words []string
	for _, t := range terms {
		words = append(words, t.word)
	}
	return words
}

func TestQueryTerms(t *testing.T) {
	cases := []struct {
		query string
		want  []string
		ok    bool
	}{
		{"harga emas antam hari ini", []string{"harga", "emas", "antam"}, true},
		{"Siapa menteri keuangan Indonesia sekarang?", []string{"menteri", "keuangan", "indonesia"}, true},
		{"libur nasional oktober 2026", []string{"libur", "nasional", "oktober"}, true},
		{"jadwal KRL bogor-jakarta kota", []string{"jadwal", "krl", "bogor", "jakarta", "kota"}, true},
		{"tarif listrik per kWh", []string{"tarif", "listrik", "kwh"}, true},
		{"site:kompas.com harga BBM -pertalite", []string{"harga", "bbm"}, true},
		{"Café Kopi Kenangan", []string{"cafe", "kopi", "kenangan"}, true},
		{"apa itu", nil, true},
		{"2026", nil, true},
		{"东京 天气", nil, false},
	}

	for _, tc := range cases {
		terms, ok := queryTerms(tc.query)
		if got := termWords(terms); ok != tc.ok || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("queryTerms(%q) = %v, %v; want %v, %v", tc.query, got, ok, tc.want, tc.ok)
		}
	}

	terms, _ := queryTerms("harga emas")
	if terms[0].weight != 0.5 || terms[1].weight != 1 {
		t.Errorf("weights = %+v, want the generic word at half weight", terms)
	}
}

// Bing, answering a suspected bot, sometimes searches only the query's first
// word. These are the result sets it returned on 2026-10-06; each looked like
// a normal answer and covered some of the query's words.
func TestFilterRelevantRejectsFirstWordDecoys(t *testing.T) {
	krl := []Result{
		{Title: "Jadwal Sholat Kota Jakarta Hari Ini", Snippet: "Jadwal sholat subuh, dzuhur, ashar, maghrib, dan isya hari ini untuk wilayah Kota Jakarta dan sekitarnya.", URL: "https://jadwal-sholat.tirto.id/kota-jakarta"},
		{Title: "Jadwal Sholat atau Waktu Sholat Jakarta dan Seluruh Indonesia", Snippet: "Diatas ini jadwal shalat fardu, semoga bisa mempermudah kita untuk sholat pada waktunya.", URL: "https://jadwalsholat.org/"},
		{Title: "Jadwal Sholat Hari Ini: Berdasarkan Lokasi Anda", Snippet: "Dapatkan jadwal sholat yang akurat untuk Subuh, Dhuhur, Ashar, Magrib, dan Isya.", URL: "https://qurantime.net/id"},
		{Title: "Jadwal Siaran Langsung Sepakbola TV Indonesia Hari Ini", Snippet: "Berikut jadwal siaran langsung pertandingan sepakbola.", URL: "https://www.goal.com/id/jadwal"},
		{Title: "Jadwal Sholat, Waktu Sholat di Indonesia | IslamicFinder", Snippet: "Dapatkan Jadwal Sholat akurat dan Azan.", URL: "https://www.islamicfinder.org/world/indonesia/"},
	}
	if _, ok := filterRelevant("jadwal krl bogor jakarta kota", krl, false); ok {
		t.Error(`prayer timetables accepted for "jadwal krl bogor jakarta kota"`)
	}

	siapa := []Result{
		{Title: "Arti kata siapa - Kamus Besar Bahasa Indonesia (KBBI) Online", Snippet: "Definisi/arti kata 'siapa' di Kamus Besar Bahasa Indonesia (KBBI) adalah pron 1 kata tanya untuk menanyakan nomina insan.", URL: "https://kbbi.web.id/siapa"},
		{Title: `Arti Kata "siapa" Menurut KBBI - Kamus Besar Bahasa Indonesia`, Snippet: "Arti Kata siapa adalah si·a·pa pron 1 kata tanya untuk menanyakan nomina insan.", URL: "https://kbbi.co.id/arti-kata/siapa"},
		{Title: "siapa - Wikikamus bahasa Indonesia", Snippet: "“siapa” di Kamus Besar Bahasa Indonesia, edisi VI (Daring), Jakarta: Badan Pengembangan dan Pembinaan Bahasa, Kementerian Pendidikan.", URL: "https://id.wiktionary.org/wiki/siapa"},
	}
	if _, ok := filterRelevant("siapa menteri keuangan indonesia sekarang", siapa, false); ok {
		t.Error(`dictionary entries for "siapa" accepted for a question about the finance minister`)
	}
}

// The generic recipe sites Bing returned for "resep rendang padang": one
// mentions "masakan padang", none mentions rendang.
func TestStrictRelevanceRequiresTheSpecificWords(t *testing.T) {
	recipes := []Result{
		{Title: "Simpan, Tulis & Bagikan Resep dengan Komunitas Masak di Seluruh Dunia", Snippet: "Ribuan resep autentik dari seluruh dunia.", URL: "https://cookpad.com/id"},
		{Title: "Resep masakan rumahan enak dan mudah - Cookpad", Snippet: "Aneka olahan masakan lezat. Cara memasak resep masakan udang, daun singkong masakan padang, aneka masakan ikan.", URL: "https://cookpad.com/id/cari/masakan"},
		{Title: "40 Resep Menu Masakan Sehari-hari agar Tidak Bosan", Snippet: "Referensi menu atau resep masakan sehari-hari.", URL: "https://id.theasianparent.com/resep-masakan"},
		{Title: "20 Resep Masakan Sehari-Hari Agar Tidak Bosan, Enak, Mudah", Snippet: "Aneka resep masakan sehari-hari.", URL: "https://www.yummy.co.id/artikel/resep"},
		{Title: "Kumpulan Resep Makanan dan Minuman Enak serta Mudah Dibuat", Snippet: "Kumpulan resep makanan dan minuman.", URL: "https://www.idntimes.com/food/recipe"},
	}

	if _, ok := filterRelevant("resep rendang padang", recipes, true); ok {
		t.Error("strict check accepted recipe sites that never mention rendang")
	}

	rendang := append([]Result{{Title: "Resep Rendang Daging Sapi Khas Padang", Snippet: "Cara memasak rendang padang yang empuk.", URL: "https://cookpad.com/id/resep/rendang"}}, recipes...)
	if kept, ok := filterRelevant("resep rendang padang", rendang, true); !ok || kept[0].URL != "https://cookpad.com/id/resep/rendang" {
		t.Errorf("strict check rejected a real answer: kept=%v ok=%v", kept, ok)
	}

	// Worded unlike its answers, a query fails the strict check but passes
	// the lenient one that the other engines use.
	paspor := []Result{{Title: "Biaya Pembuatan Paspor 2026", Snippet: "Biaya paspor biasa Rp350.000.", URL: "https://www.imigrasi.go.id/biaya"}}
	if _, ok := filterRelevant("biaya bikin paspor", paspor, true); ok {
		t.Error(`strict check accepted results missing "bikin"; the test's premise changed`)
	}
	if _, ok := filterRelevant("biaya bikin paspor", paspor, false); !ok {
		t.Error("lenient check rejected an answer worded differently from the query")
	}
}

func TestFilterRelevantAcceptsRealAnswers(t *testing.T) {
	cases := []struct {
		query   string
		results []Result
	}{
		{"jadwal krl bogor jakarta kota", []Result{
			{Title: "Jadwal KRL Terbaru", Snippet: "1. Bogor - Cilebut - Bojong Gede - Citayam - Depok - Manggarai - Jakarta Kota", URL: "https://jadwalkrl.com/"},
		}},
		{"siapa menteri keuangan indonesia sekarang", []Result{
			{Title: "Menteri Keuangan Republik Indonesia", Snippet: "Menteri Keuangan saat ini adalah Purbaya Yudhi Sadewa.", URL: "https://www.kemenkeu.go.id/profil/menteri"},
		}},
		{"biaya pembuatan paspor 2026", []Result{
			{Title: "Update Biaya Paspor 2026: Segini Budget yang Harus Kamu Siapin", Snippet: "Mengurus paspor jadi langkah penting.", URL: "https://www.medcom.id/paspor"},
			{Title: "Biaya Pembuatan Paspor 2026: Paspor Biasa & E-Paspor", Snippet: "Mengetahui biaya pembuatan paspor 2026 sejak awal.", URL: "https://biro-jasa.org/paspor"},
		}},
		// An English query answered in Indonesian still clears the bar.
		{"iPhone 17 price Indonesia", []Result{
			{Title: "Harga iPhone 17 di Indonesia", Snippet: "Harga resmi iPhone 17 mulai Rp16 juta.", URL: "https://www.ibox.co.id/iphone-17"},
		}},
		{"syarat perpanjangan SIM online", []Result{
			{Title: "Cara Perpanjang SIM Online 2026 dan Biaya-Syaratnya", Snippet: "Panduan lengkap.", URL: "https://www.detik.com/sim"},
		}},
	}

	for _, tc := range cases {
		if _, ok := filterRelevant(tc.query, tc.results, true); !ok {
			t.Errorf("filterRelevant(%q) rejected a real answer", tc.query)
		}
	}
}

func TestFilterRelevantShortTermsMatchWholeWords(t *testing.T) {
	results := []Result{{Title: "Simak cara mudah menabung", URL: "https://contoh.id/simak"}}

	if _, ok := filterRelevant("perpanjang sim", results, false); ok {
		t.Error(`"sim" matched inside "simak"; short terms must match whole words`)
	}

	results = []Result{{Title: "Syarat perpanjang SIM online 2026", URL: "https://contoh.id/sim"}}
	if _, ok := filterRelevant("perpanjang sim", results, false); !ok {
		t.Error("a result naming both terms was judged unrelated")
	}
}

func TestFilterRelevantReadsTheAddressToo(t *testing.T) {
	results := []Result{{Title: "Beranda", Snippet: "Selamat datang", URL: "https://www.tokopedia.com/"}}

	if _, ok := filterRelevant("tokopedia", results, true); !ok {
		t.Error("a result whose address names the query was judged unrelated")
	}
}

func TestFilterRelevantSkipsQueriesItCannotJudge(t *testing.T) {
	results := []Result{{Title: "Unrelated", URL: "https://example.com"}}

	for _, query := range []string{"apa itu", "2026", "東京"} {
		if kept, ok := filterRelevant(query, results, true); !ok || len(kept) != 1 {
			t.Errorf("filterRelevant(%q) = %v, %v; want the results untouched", query, kept, ok)
		}
	}
}
