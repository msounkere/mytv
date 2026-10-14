package epg

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mytv/internal/domain"
)

var maintenantEssai = utc(10, 12, 0)

type banc struct {
	t    *testing.T
	base *Base
	f    *fauxFournisseur
	h    *horlogeFausse
	svc  *Service
	logs *tampon
}

// tampon est un io.Writer sur lequel on peut lire pendant que slog ecrit.
type tampon struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (t *tampon) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.b.Write(p)
}
func (t *tampon) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.b.String()
}

func chainesEssai() listeChaines {
	return listeChaines{
		{ID: "f:1", FournisseurID: "f", Nom: "FR: TF1 HD", EpgID: "tf1.fr"}, // casse differente
		{ID: "f:2", FournisseurID: "f", Nom: "France 2 FHD"},                // par nom
		{ID: "f:3", FournisseurID: "f", Nom: "|FR| Canal+ HD"},
		{ID: "f:4", FournisseurID: "f", Nom: "Télé-Éclair"},
		{ID: "f:5", FournisseurID: "f", Nom: "Arte"}, // canal sans aucun programme
		{ID: "f:6", FournisseurID: "f", Nom: "Eurosport 1"},
		{ID: "f:7", FournisseurID: "f", Nom: "Eurosport"},                           // ambigu
		{ID: "f:8", FournisseurID: "f", Nom: "Chaine fantome", EpgID: "fantome.fr"}, // canal jamais declare
		{ID: "f:9", FournisseurID: "f", Nom: "Aucune correspondance"},
		{ID: "g:1", FournisseurID: "g", Nom: "TF1"}, // autre fournisseur : jamais touche
	}
}

func nouveauBanc(t *testing.T, mod func(*Source, *Options)) *banc {
	t.Helper()
	b := &banc{t: t, h: &horlogeFausse{now: maintenantEssai}, logs: &tampon{}}
	var err error
	b.base, err = Ouvrir(context.Background(), filepath.Join(t.TempDir(), "epg.db"), b.h)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.base.Fermer() })
	guide := lireGuide(t)
	b.f = &fauxFournisseur{id: "f", corps: func() []byte { return guide }}
	src := Source{Fournisseur: b.f, Actif: true, AgeMax: 24 * time.Hour, DelaiMax: time.Minute, Reprise: 10 * time.Minute}
	opt := Options{Horloge: b.h, Journal: slog.New(slog.NewTextHandler(b.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Passe: 6 * time.Hour, Futur: 24 * time.Hour, TailleMax: 64 << 20}
	if mod != nil {
		mod(&src, &opt)
	}
	b.svc, err = Nouveau(chainesEssai(), b.base, []Source{src}, opt)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (b *banc) grille(chaine domain.ContenuID) []domain.Programme {
	b.t.Helper()
	g, err := b.base.Grille(context.Background(), []domain.ContenuID{chaine}, utc(1, 0, 0), utc(30, 0, 0))
	if err != nil {
		b.t.Fatal(err)
	}
	return g[chaine]
}

func (b *banc) nbProgrammes() int {
	var n int
	if err := b.base.db.QueryRow(`SELECT COUNT(*) FROM epg_programmes`).Scan(&n); err != nil {
		b.t.Fatal(err)
	}
	return n
}

// CA1 + CA2 + CA3 + CA4 de bout en bout : un guide realiste est telecharge, filtre,
// rapproche et stocke.
func TestRafraichirIngereLeGuide(t *testing.T) {
	b := nouveauBanc(t, nil)
	if err := b.svc.Rafraichir(context.Background(), "f"); err != nil {
		t.Fatal(err)
	}
	// TF1 (epg_id a la casse pres) : doublon retire, fin manquante deduite (13:00 -> 14:00).
	tf1 := b.grille("f:1")
	if !egal(titres(tf1), []string{"Journal & meteo", "Film de midi", "Sans fin connue", "Apres-midi"}) {
		t.Fatalf("TF1 = %v", titres(tf1))
	}
	if !tf1[2].Fin.Equal(utc(10, 14, 0)) {
		t.Fatalf("fin deduite = %v", tf1[2].Fin)
	}
	// Par nom normalise.
	for chaine, titre := range map[domain.ContenuID]string{"f:2": "Telematin", "f:3": "Debat", "f:4": "Emission accentuee", "f:8": "Canal non declare"} {
		if g := b.grille(chaine); len(g) == 0 || g[0].Titre != titre {
			t.Errorf("%s = %v, attendu %q en tete", chaine, titres(g), titre)
		}
	}
	// Titre vide : le programme est conserve (la description existe).
	if g := b.grille("f:2"); len(g) != 2 || g[1].Titre != "" || g[1].Description != "Titre vide" {
		t.Errorf("France 2 = %+v", g)
	}
	// Chaine sans programme, ambigue, sans correspondance : rien, sans erreur.
	for _, id := range []domain.ContenuID{"f:5", "f:6", "f:7", "f:9"} {
		if g := b.grille(id); len(g) != 0 {
			t.Errorf("%s ne devrait rien recevoir : %v", id, titres(g))
		}
	}
	// « Eurosport 1 » ne correspond a aucun nom ; « Eurosport » est ambigu -> signale.
	if a, _ := b.base.Ambigus(context.Background(), "f"); len(a) != 1 || a[0] != "f:7" {
		t.Errorf("ambigus = %v", a)
	}
	// L'autre fournisseur n'a pas ete touche.
	if g := b.grille("g:1"); len(g) != 0 {
		t.Error("g:1 appartient a un autre fournisseur")
	}
	// Memoire/base : les programmes des canaux sans chaine (inconnue.xx, ambigus) ne sont pas stockes.
	if n := b.nbProgrammes(); n != 4+2+1+1+1 { // TF1, France 2, Canal+, Telecl, fantome
		t.Errorf("programmes stockes = %d", n)
	}
	if d, _ := b.base.DerniereSynchroEPG(context.Background(), "f"); !d.Equal(maintenantEssai) {
		t.Errorf("synchro = %v", d)
	}
	// Journal : volumes, jamais l'URL ni le mot de passe.
	if l := b.logs.String(); !strings.Contains(l, "programmes_gardes=9") || !strings.Contains(l, "ambigues=1") {
		t.Errorf("journal sans volumes :\n%s", l)
	}
}

// CA1 : un guide .xml.gz servi tel quel (sans Content-Encoding) est decompresse.
func TestRafraichirGuideGzip(t *testing.T) {
	b := nouveauBanc(t, nil)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write(lireGuide(t))
	gz.Close()
	b.f.corps = func() []byte { return buf.Bytes() }
	if err := b.svc.Rafraichir(context.Background(), "f"); err != nil {
		t.Fatal(err)
	}
	if b.nbProgrammes() != 9 {
		t.Fatalf("programmes = %d", b.nbProgrammes())
	}
}

// CA4 : telechargement en echec, guide corrompu, guide trop gros, panne d'ecriture :
// dans tous les cas l'ancien EPG reste servi et l'erreur est renvoyee.
func TestEchecConserveLAncienEPG(t *testing.T) {
	guide := lireGuide(t)
	cas := map[string]func(b *banc){
		"telechargement": func(b *banc) { b.f.erreur = domain.ErrAmont },
		"xml tronque":    func(b *banc) { b.f.corps = func() []byte { return guide[:len(guide)/2] } },
		"trop gros":      func(b *banc) { b.svc.opt.TailleMax = 500 },
		"ecriture": func(b *banc) {
			b.base.apresInsertion = func(table string) error {
				if table == "epg_programmes" {
					return errors.New("panne injectee")
				}
				return nil
			}
		},
	}
	for nom, panne := range cas {
		t.Run(nom, func(t *testing.T) {
			b := nouveauBanc(t, nil)
			if err := b.svc.Rafraichir(context.Background(), "f"); err != nil {
				t.Fatal(err)
			}
			avant := len(b.grille("f:1"))
			b.h.Avancer(48 * time.Hour)
			panne(b)
			err := b.svc.Rafraichir(context.Background(), "f")
			if err == nil {
				t.Fatal("erreur attendue")
			}
			if strings.Contains(err.Error(), motDePasseSecret) || strings.Contains(b.logs.String(), motDePasseSecret) ||
				strings.Contains(b.logs.String(), "password") {
				t.Fatalf("secret divulgue : %v\n%s", err, b.logs.String())
			}
			b.base.apresInsertion = nil
			if got := len(b.grille("f:1")); got != avant || b.nbProgrammes() != 9 {
				t.Fatalf("ancien EPG altere : %d programmes pour TF1 (avant %d), %d au total", got, avant, b.nbProgrammes())
			}
			if d, _ := b.base.DerniereSynchroEPG(context.Background(), "f"); !d.Equal(maintenantEssai) {
				t.Fatalf("la date de synchro a bouge : %v", d)
			}
		})
	}
}

// CA6 : rafraichissement.epg respecte -- SiPerime ne telecharge pas un EPG frais.
func TestSiPerimeRespecteLAgeMax(t *testing.T) {
	ctx := context.Background()
	b := nouveauBanc(t, nil)
	if ok, err := b.svc.SiPerime(ctx, "f"); err != nil || !ok {
		t.Fatalf("premier appel : %v %v", ok, err)
	}
	b.h.Avancer(23 * time.Hour)
	if ok, err := b.svc.SiPerime(ctx, "f"); err != nil || ok {
		t.Fatalf("EPG frais : telecharge=%v %v", ok, err)
	}
	b.h.Avancer(time.Hour) // exactement AgeMax
	if ok, err := b.svc.SiPerime(ctx, "f"); err != nil || !ok {
		t.Fatalf("EPG perime : telecharge=%v %v", ok, err)
	}
	if n := b.f.appels.Load(); n != 2 {
		t.Fatalf("telechargements = %d", n)
	}
	// Forcer telecharge meme si frais.
	if err := b.svc.Rafraichir(ctx, "f"); err != nil || b.f.appels.Load() != 3 {
		t.Fatalf("Rafraichir force : %v, %d", err, b.f.appels.Load())
	}
}

// CA6 : des demandes simultanees pour un meme fournisseur ne font qu'UN telechargement.
func TestSingleflightParFournisseur(t *testing.T) {
	b := nouveauBanc(t, nil)
	b.f.bloque = make(chan struct{})
	b.f.entre = make(chan struct{}, 10)
	b.f.corps = func() []byte { return lireGuide(t) }
	// Le corps bloque jusqu'a la fermeture de `bloque`, puis est vide : le test ne
	// regarde que le nombre d'appels amont.
	const n = 6
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- b.svc.Rafraichir(context.Background(), "f")
		}()
	}
	<-b.f.entre
	// Laisse aux autres le temps de se joindre au vol en cours.
	attendre(t, "demandeurs joints", func() bool {
		b.svc.mu.Lock()
		defer b.svc.mu.Unlock()
		v := b.svc.vols["f"]
		return v != nil && v.attendants == n
	})
	close(b.f.bloque)
	wg.Wait()
	close(errs)
	if got := b.f.appels.Load(); got != 1 {
		t.Fatalf("telechargements = %d, attendu 1", got)
	}
	for range errs {
	}
}

// CA6 : quand le dernier demandeur part, le telechargement est interrompu (pas de goroutine orpheline).
func TestAnnulationDuDernierDemandeurInterromptLeVol(t *testing.T) {
	b := nouveauBanc(t, nil)
	b.f.bloque = make(chan struct{}) // jamais ferme
	b.f.entre = make(chan struct{}, 1)
	ctx, annuler := context.WithCancel(context.Background())
	fini := make(chan error, 1)
	go func() { fini <- b.svc.Rafraichir(ctx, "f") }()
	<-b.f.entre
	annuler()
	if err := <-fini; !errors.Is(err, context.Canceled) {
		t.Fatalf("erreur = %v", err)
	}
	attendre(t, "vol retire", func() bool {
		b.svc.mu.Lock()
		defer b.svc.mu.Unlock()
		return len(b.svc.vols) == 0
	})
}

func TestFournisseurInconnuOuInactif(t *testing.T) {
	b := nouveauBanc(t, func(s *Source, _ *Options) { s.Actif = false })
	if err := b.svc.Rafraichir(context.Background(), "x"); !errors.Is(err, ErrInconnu) {
		t.Fatalf("erreur = %v", err)
	}
	if err := b.svc.Rafraichir(context.Background(), "f"); !errors.Is(err, ErrInactif) {
		t.Fatalf("erreur = %v", err)
	}
	if err := b.svc.RafraichirTous(context.Background()); err != nil || b.f.appels.Load() != 0 {
		t.Fatalf("un inactif n'est jamais rafraichi : %v", err)
	}
}

// CA6 : la planification rafraichit au demarrage (EPG perime), puis a chaque AgeMax,
// retente apres Reprise en cas d'echec, et s'arrete proprement.
func TestPlanifierRespecteLaFrequence(t *testing.T) {
	b := nouveauBanc(t, nil)
	ctx, annuler := context.WithCancel(context.Background())
	fini := make(chan struct{})
	go func() { b.svc.Planifier(ctx); close(fini) }()

	attendre(t, "refresh au demarrage", func() bool { return b.f.appels.Load() == 1 && b.h.enAttente() >= 2 })
	b.h.Avancer(23 * time.Hour)
	time.Sleep(20 * time.Millisecond)
	if b.f.appels.Load() != 1 {
		t.Fatal("rafraichi avant l'age maximal")
	}
	b.h.Avancer(time.Hour)
	attendre(t, "refresh a l'echeance", func() bool { return b.f.appels.Load() == 2 })

	// Echec : nouvel essai apres Reprise (10 min), pas apres AgeMax.
	attendre(t, "minuteurs armes", func() bool { return b.h.enAttente() >= 2 })
	b.f.erreur = domain.ErrAmont
	b.h.Avancer(24 * time.Hour)
	attendre(t, "essai echoue", func() bool { return b.f.appels.Load() == 3 })
	attendre(t, "minuteur de reprise", func() bool { return b.h.enAttente() >= 2 })
	b.f.erreur = nil
	b.h.Avancer(10 * time.Minute)
	attendre(t, "reprise", func() bool { return b.f.appels.Load() == 4 })

	annuler()
	select {
	case <-fini:
	case <-time.After(5 * time.Second):
		t.Fatal("Planifier ne s'est pas arrete")
	}
}

// Pas de redemarrage martelant : un EPG deja frais n'est pas retelecharge au demarrage.
func TestPlanifierNeRetelechargePasUnEPGFrais(t *testing.T) {
	b := nouveauBanc(t, nil)
	if err := b.svc.Rafraichir(context.Background(), "f"); err != nil {
		t.Fatal(err)
	}
	ctx, annuler := context.WithCancel(context.Background())
	fini := make(chan struct{})
	go func() { b.svc.Planifier(ctx); close(fini) }()
	attendre(t, "planificateurs armes", func() bool { return b.h.enAttente() >= 2 })
	if n := b.f.appels.Load(); n != 1 {
		t.Fatalf("telechargements = %d", n)
	}
	annuler()
	<-fini
}

// Purge : la planification retire periodiquement les programmes termines.
func TestPlanifierPurgeLesProgrammesTermines(t *testing.T) {
	b := nouveauBanc(t, func(s *Source, o *Options) { s.AgeMax = 1000 * time.Hour })
	b.svc.Rafraichir(context.Background(), "f")
	ctx, annuler := context.WithCancel(context.Background())
	fini := make(chan struct{})
	go func() { b.svc.Planifier(ctx); close(fini) }()
	attendre(t, "planificateurs armes", func() bool { return b.h.enAttente() >= 2 })
	avant := b.nbProgrammes()
	b.h.Avancer(5 * time.Hour) // 17:00, passe=6h -> purge ce qui a fini avant 11:00
	attendre(t, "purge", func() bool { return b.nbProgrammes() < avant })
	annuler()
	<-fini
}

func TestNouveauRefuseLesReglagesIncoherents(t *testing.T) {
	f := &fauxFournisseur{id: "f", corps: func() []byte { return nil }}
	ok := Source{Fournisseur: f, Actif: true, AgeMax: time.Hour, DelaiMax: time.Minute, Reprise: time.Minute}
	base := Options{Passe: time.Hour, Futur: time.Hour, TailleMax: 1}
	cas := map[string]struct {
		src Source
		opt Options
	}{
		"futur nul":    {ok, Options{Passe: time.Hour, TailleMax: 1}},
		"taille nulle": {ok, Options{Passe: time.Hour, Futur: time.Hour}},
		"sans delai":   {Source{Fournisseur: f, Actif: true}, base},
	}
	for nom, c := range cas {
		if _, err := Nouveau(nil, nil, []Source{c.src}, c.opt); err == nil {
			t.Errorf("%s : erreur attendue", nom)
		}
	}
	if _, err := Nouveau(nil, nil, []Source{ok, ok}, base); err == nil {
		t.Error("fournisseur en double accepte")
	}
	sansGuide := &fournisseurSansGuide{}
	if _, err := Nouveau(nil, nil, []Source{{Fournisseur: sansGuide, Actif: true, AgeMax: 1, DelaiMax: 1, Reprise: 1}}, base); !errors.Is(err, ErrSansGuide) {
		t.Errorf("erreur = %v", err)
	}
}

type fournisseurSansGuide struct{ fauxFournisseur }

// Masque URLEPG : ce fournisseur n'implemente pas domain.SourceEPG.
func (*fournisseurSansGuide) ID() string { return "sg" }
func (*fournisseurSansGuide) URLEPG()    {}
