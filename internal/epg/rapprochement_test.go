package epg

import (
	"testing"

	"mytv/internal/domain"
)

func ch(n int, nom, epg string) domain.Chaine {
	return domain.Chaine{ID: domain.ContenuID("f:" + string(rune('0'+n))), FournisseurID: "f", Nom: nom, EpgID: epg}
}

func parChaine(r []domain.Rapprochement) map[domain.ContenuID]domain.Rapprochement {
	m := map[domain.ContenuID]domain.Rapprochement{}
	for _, x := range r {
		m[x.Chaine] = x
	}
	return m
}

var canauxEssai = []Canal{
	{ID: "TF1.fr", Noms: []string{"TF1"}},
	{ID: "canalplus.fr", Noms: []string{"Canal+", "Canal Plus"}},
	{ID: "telecl.fr", Noms: []string{"Télé-Éclair"}},
	{ID: "eurosport.fr", Noms: []string{"Eurosport"}},
	{ID: "eurosport.de", Noms: []string{"Eurosport"}},
	{ID: "arte.fr", Noms: []string{"Arte"}},
}

// CA3 : l'epg_id du fournisseur prime, meme si le nom pointerait ailleurs.
func TestRapprochementParEpgIDPrime(t *testing.T) {
	r := parChaine(Rapprocher([]domain.Chaine{ch(1, "Arte", "TF1.fr")}, canauxEssai))
	x := r["f:1"]
	if x.EpgID != "TF1.fr" || x.Methode != domain.RapprocheParEpgID || x.Ambigu {
		t.Fatalf("rapprochement = %+v", x)
	}
}

// CA3 : a defaut d'epg_id (vide, ou inconnu du guide), le nom normalise decide :
// casse, accents, prefixes pays et suffixes de qualite sont ignores.
func TestRapprochementParNomNormalise(t *testing.T) {
	r := parChaine(Rapprocher([]domain.Chaine{
		ch(1, "|FR| TF1 FHD", ""),
		ch(2, "FR: CANAL+ HD", "inconnu.fr"),
		ch(3, "tele eclair", ""),
		ch(4, "ARTE HD", ""),
	}, canauxEssai))
	att := map[domain.ContenuID]string{"f:1": "TF1.fr", "f:2": "canalplus.fr", "f:3": "telecl.fr", "f:4": "arte.fr"}
	for id, epg := range att {
		if x := r[id]; x.EpgID != epg || x.Methode != domain.RapprocheParNom || x.Ambigu {
			t.Errorf("%s : %+v, attendu %s par nom", id, x, epg)
		}
	}
}

// CA3 : deux canaux du guide portent le meme nom -> on signale, on ne devine pas.
func TestRapprochementAmbiguNeDevinePas(t *testing.T) {
	r := parChaine(Rapprocher([]domain.Chaine{ch(1, "Eurosport HD", "")}, canauxEssai))
	x, ok := r["f:1"]
	if !ok || !x.Ambigu || x.EpgID != "" {
		t.Fatalf("rapprochement = %+v", x)
	}
}

// CA3 : sans correspondance, aucune ligne ; epg_id different par la casse seulement tolere.
func TestRapprochementIntrouvableEtCasseDeLEpgID(t *testing.T) {
	r := parChaine(Rapprocher([]domain.Chaine{ch(1, "Chaine Perdue", ""), ch(2, "Autre nom", "tf1.FR")}, canauxEssai))
	if _, ok := r["f:1"]; ok {
		t.Fatal("une chaine introuvable ne doit pas avoir de rapprochement")
	}
	if x := r["f:2"]; x.EpgID != "TF1.fr" || x.Methode != domain.RapprocheParEpgID {
		t.Fatalf("casse de l'epg_id : %+v", x)
	}
}

// Deux chaines du catalogue (HD et SD) peuvent partager un meme canal : ce n'est PAS une ambiguite.
func TestDeuxChainesPourUnCanalNeSontPasAmbigues(t *testing.T) {
	r := parChaine(Rapprocher([]domain.Chaine{ch(1, "TF1", ""), ch(2, "TF1 HD", "")}, canauxEssai))
	if r["f:1"].EpgID != "TF1.fr" || r["f:2"].EpgID != "TF1.fr" || r["f:1"].Ambigu || r["f:2"].Ambigu {
		t.Fatalf("%+v", r)
	}
}
