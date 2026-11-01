package preparation

import (
	"context"
	"strings"
	"testing"
	"time"

	"mytv/internal/baux"
	"mytv/internal/domain"
	"mytv/internal/testutil/fauxiptv"
)

func (b *banc) sonde(g *baux.Gestionnaire, m *Moteur) *Sonde {
	b.t.Helper()
	o := OptionsSonde{Fournisseurs: map[string]domain.Fournisseur{"iptva": b.client}, Stockage: b.stockage, Baux: g, Occupation: g,
		Prepares: m, Suspendre: m.Suspendre, Journal: journalTest(b.t),
		Debit: func(ctx context.Context, id, serie domain.ContenuID) int {
			numero := strings.TrimPrefix(string(id), "iptva:")
			d, _ := b.client.DebitFilm(ctx, numero)
			return d
		}}
	s, err := NouvelleSonde(o)
	if err != nil {
		b.t.Fatal(err)
	}
	return s
}

// CA6 : trois lectures de 256 Kio a 0 %, 50 % et 90 %, SEQUENTIELLES (FilmsPic = 1) ;
// `immediat` quand le debit mesure >= 2 x le debit du fichier (info.bitrate) ; resultat garde
// 10 min (aucune nouvelle requete) ; `pret` pour un fichier prepare ; `inconnu` (sans la
// moindre requete) pendant une lecture ; 404 pour un contenu inconnu.
func TestCA6_SondeSequentielleImmediatEtCache(t *testing.T) {
	g := gestionnaireBaux(t)
	b := nouveauBanc(t, fauxiptv.Options{TailleFilm: 2 << 20,
		InfosFilm: map[string]any{"2001": map[string]any{"info": map[string]any{"bitrate": 4400}}}}, nil, g)
	g.AvecVeilleur(b.m.Veilleur())
	s := b.sonde(g, b.m)
	ctx := context.Background()

	r, err := s.Disponibilite(ctx, "s1", "iptva:2001", "")
	if err != nil || r.Etat != "immediat" || r.DebitFichierKbps != 4400 || r.DebitKbps < 2*4400 || r.Mesure.IsZero() {
		t.Fatalf("resultat = %+v, %v", r, err)
	}
	c := b.faux.Compteurs()
	if c.Films != 3 || c.FilmsPic != 1 || c.ParAction["get_vod_info"] != 1 {
		t.Errorf("requetes : films %d, pic %d, infos %d", c.Films, c.FilmsPic, c.ParAction["get_vod_info"])
	}
	vus := rangesVus(b.faux)
	if !egal(vus, []string{"bytes=0-262143", "bytes=1048576-1310719", "bytes=1887436-2097151"}) {
		t.Errorf("plages lues = %v", vus)
	}
	if g.Occupes("iptva") != 0 {
		t.Error("place non rendue apres la mesure")
	}
	// Garde 10 min : aucune nouvelle requete.
	r2, _ := s.Disponibilite(ctx, "s1", "iptva:2001", "")
	if r2 != r || b.faux.Compteurs().Films != 3 {
		t.Errorf("resultat non garde : %+v, films %d", r2, b.faux.Compteurs().Films)
	}
	// Une lecture en cours (place tenue par une autre session) : inconnu, sans requete.
	bail, err := g.Acquerir(ctx, "iptva", "s2", "iptva:2002")
	if err != nil {
		t.Fatal(err)
	}
	b.faux.RazCompteurs()
	if r, err := s.Disponibilite(ctx, "s1", "iptva:2002", ""); err != nil || r.Etat != "inconnu" || r.DebitKbps != 0 {
		t.Errorf("pendant une lecture : %+v, %v", r, err)
	}
	if c := b.faux.Compteurs(); c.Films != 0 || c.API != 0 {
		t.Errorf("requetes pendant une lecture : films %d, api %d", c.Films, c.API)
	}
	bail.Liberer()
	// Fichier prepare : pret, sans requete.
	b.liste.AjouterAVoir(ctx, "p1", domain.ElementAVoir{Contenu: "iptva:2002"})
	b.m.cycle(ctx)
	b.faux.RazCompteurs()
	if r, _ := s.Disponibilite(ctx, "s1", "iptva:2002", ""); r.Etat != "pret" || b.faux.Compteurs().Films != 0 {
		t.Errorf("fichier prepare : %+v, films %d", r, b.faux.Compteurs().Films)
	}
	if _, err := s.Disponibilite(ctx, "s1", "iptva:4242", ""); err == nil {
		t.Error("contenu inconnu accepte")
	}
}

// CA6 : `lent` quand le fournisseur sert sous 2 x le debit du fichier (8 Mbit/s par defaut
// quand il ne le dit pas) ; la preparation en cours est suspendue pendant la mesure (jamais
// deux connexions) puis reprend.
func TestCA6_SondeLenteEtPreparationSuspendue(t *testing.T) {
	g := gestionnaireBaux(t)
	b := nouveauBanc(t, fauxiptv.Options{TailleFilm: 1 << 20, DebitFilm: 512 << 10}, nil, g)
	g.AvecVeilleur(b.m.Veilleur())
	s := b.sonde(g, b.m)
	ctx := context.Background()
	// La preparation telecharge 2002 (2 s a 512 Kio/s).
	b.liste.AjouterAVoir(ctx, "p1", domain.ElementAVoir{Contenu: "iptva:2002"})
	b.lancer(b.m)
	b.attendre(func() bool { return b.faux.FilmsOuverts() == 1 }, 3*time.Second, "preparation non demarree")
	time.Sleep(200 * time.Millisecond)

	r, err := s.Disponibilite(ctx, "s1", "iptva:2001", "")
	if err != nil || r.Etat != "lent" || r.DebitFichierKbps != 8000 || r.DebitKbps <= 0 || r.DebitKbps >= 16000 {
		t.Fatalf("resultat = %+v, %v", r, err)
	}
	b.attendre(func() bool { return b.etatDe("iptva:2002") == domain.PreparationPrete }, 10*time.Second, "preparation jamais reprise")
	if pic := b.faux.Compteurs().FilmsPic; pic != 1 {
		t.Errorf("FilmsPic = %d : la sonde et la preparation se sont chevauchees", pic)
	}
	if b.instr.valeur("preparation_pauses_total") < 1 {
		t.Error("la preparation n'a pas ete suspendue")
	}
}
