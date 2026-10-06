package textmatch

import "testing"

func TestFold(t *testing.T) {
	if got := Fold("Kurs-Rupiah, Hari Ini! (Résumé)"); got != "kurs rupiah hari ini resume" {
		t.Errorf("Fold = %q", got)
	}
}

func TestHas(t *testing.T) {
	text := NewText("Syarat perpanjang SIM: harganya naik, Kementerian Keuangan menetapkan tarif")

	for _, term := range []string{"sim", "harga", "perpanjangan", "keuangan", "syarat"} {
		if !text.Has(term) {
			t.Errorf("Has(%q) = false", term)
		}
	}
	for _, term := range []string{"menteri", "si", "tari", "simak"} {
		if text.Has(term) {
			t.Errorf("Has(%q) = true", term)
		}
	}

	if got := NewText("Simak cara perpanjang SIM").Has("sim"); !got {
		t.Error("whole-word short term not found")
	}
	if got := NewText("Simak caranya").Has("sim"); got {
		t.Error(`"sim" found inside "simak"`)
	}
}

func TestCount(t *testing.T) {
	text := NewText("Harga emas naik. Harganya Rp1,9 juta; emas Antam, emasnya")
	if got := text.Count("harga"); got != 2 {
		t.Errorf("Count(harga) = %d, want 2", got)
	}
	if got := text.Count("emas"); got != 3 {
		t.Errorf("Count(emas) = %d, want 3", got)
	}
}

// Search judges relevance and fetchpage picks passages from many goroutines
// at once; a shared transformer once panicked under exactly that.
func TestFoldIsSafeForConcurrentUse(t *testing.T) {
	inputs := []string{"Kurs-Rupiah, Hari Ini!", "Café Kopi Kenangan", "Résumé à la carte", "harga emas antam"}
	done := make(chan struct{})
	for g := range 16 {
		go func() {
			defer func() { done <- struct{}{} }()
			for i := range 500 {
				in := inputs[(g+i)%len(inputs)]
				if got := Fold(in); got == "" {
					t.Errorf("Fold(%q) is empty", in)
					return
				}
			}
		}()
	}
	for range 16 {
		<-done
	}
}
