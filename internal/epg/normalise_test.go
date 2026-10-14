package epg

import "testing"

func TestNormaliserCasseAccentsPrefixesEtQualite(t *testing.T) {
	cas := map[string]string{
		"TF1":                 "tf1",
		"TF1 HD":              "tf1",
		"|FR| TF1 FHD":        "tf1",
		"FR: TF1":             "tf1",
		"[FR] TF1 4K":         "tf1",
		"FR - TF1":            "tf1",
		"Télé-Éclair":         "tele eclair",
		"  Canal+  ":          "canal plus",
		"BEIN SPORTS 1 HD":    "bein sports 1",
		"beIN Sports 1":       "bein sports 1",
		"AB1":                 "ab1",      // « ab » n'est pas un prefixe pays
		"FRANCE 2":            "france 2", // « fr » sans separateur reste
		"TF1 +1":              "tf1 plus 1",
		"HEVC H265 RAW":       "",
		"Zürich Çà œil Ñandú": "zurich ca eil nandu",
		"":                    "",
	}
	for in, attendu := range cas {
		if got := Normaliser(in); got != attendu {
			t.Errorf("Normaliser(%q) = %q, attendu %q", in, got, attendu)
		}
	}
}
