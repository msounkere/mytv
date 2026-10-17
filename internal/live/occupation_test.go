package live

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mytv/internal/baux"
	"mytv/internal/config"
	"mytv/internal/occupation"
	"mytv/internal/testutil/fauxiptv"
	"mytv/internal/testutil/instrum"
	"mytv/internal/xtream"
)

// US-34 : scenarios de bout en bout (vrai client Xtream, vrais baux, faux fournisseur
// programmable). Les durees sont celles du produit divisees par ~100 : la patience de 40 s
// devient 400 ms, le retrait 3 s -> 30 ms, plafond 30 s -> 200 ms.

type banOccupation struct {
	*integ
	suivi   *occupation.Suivi
	mesures *instrum.Enregistreur
	refus   atomic.Int32 // appels a SignalerRefus (compte declare refuse)
}

// nouvelleOccupation assemble le relais avec le diagnostic. bauxMax : places des baux mytv.
func nouvelleOccupation(t *testing.T, bauxMax int, retrait bool) *banOccupation {
	t.Helper()
	b := &banOccupation{mesures: &instrum.Enregistreur{}}
	b.integ = nouvelleIntegration(t, fauxiptv.Options{}, func(o *Options) {
		// Pas de playlist de secours : elle masquerait les 503 que ces scenarios observent.
		o.Live.PeremptionSecours = config.Duree(time.Millisecond)
		o.Bail.PatienceMax = config.Duree(400 * time.Millisecond)
		o.Bail.Reessai = config.Duree(30 * time.Millisecond)
		o.Bail.ReessaiMax = config.Duree(30 * time.Millisecond) // pause constante
		if retrait {
			o.Bail.ReessaiMax = config.Duree(200 * time.Millisecond)
		}
		g, err := baux.Nouveau(o.Bail, map[string]int{"iptva": bauxMax}, nil)
		if err != nil {
			t.Fatal(err)
		}
		o.Baux = g
		four := o.Fournisseur.(*xtream.Client)
		b.suivi = occupation.Nouveau(occupation.Options{
			Reglages: occupation.Reglages{
				CacheOccupation: 15 * time.Second, DelaiInfo: 2 * time.Second,
				RetryApres: 15 * time.Second, RetryApresMax: 60 * time.Second,
				GardeFournisseur: 5 * time.Second, SuccesPrise: 2, FenetrePrise: 30 * time.Second,
			},
			Sources: map[string]occupation.Source{"iptva": {
				Info: func(ctx context.Context) (occupation.Info, error) {
					i, err := four.InfoCompte(ctx)
					return occupation.Info{ConnexionsMax: i.ConnexionsMax, ConnexionsActives: i.ConnexionsActives}, err
				},
				AutresBaux: func(s string) int { return g.OccupesParAutres("iptva", s) },
				Occupes:    func() int { return g.Occupes("iptva") },
			}},
			Journal: o.Journal, Instrumentation: b.mesures,
		})
		t.Cleanup(b.suivi.Fermer)
		o.Occupation = b.suivi
		o.SignalerRefus = func(error) { b.refus.Add(1) }
	})
	return b
}

func (b *banOccupation) code(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var corps struct{ Code string }
	_ = json.Unmarshal(rec.Body.Bytes(), &corps)
	if en := rec.Header().Get("X-Mytv-Erreur"); en != corps.Code {
		t.Errorf("X-Mytv-Erreur %q different du code JSON %q", en, corps.Code)
	}
	return corps.Code
}

func (b *banOccupation) userInfo() int { return b.faux.Compteurs().ParAction[""] }

// US-34 CA1/CA5 : un occupant externe tient la place ; mytv ne martele pas, nomme la cause
// (compte_utilise_ailleurs), n'annonce pas de Retry-After avant l'echeance puis reprend
// tout seul quand la place est rendue.
func TestCA5_OccupantExterneLibereReprendSansMartelage(t *testing.T) {
	b := nouvelleOccupation(t, 1, true)
	b.faux.OccuperPar(time.Hour)

	rec := b.get(PrefixeParDefaut+"/1001.m3u8", "s1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("503 attendu : %d %s", rec.Code, rec.Body.String())
	}
	if c := b.code(t, rec); c != "compte_utilise_ailleurs" {
		t.Errorf("code = %q", c)
	}
	if ra := rec.Header().Get("Retry-After"); ra != "15" {
		t.Errorf("premier Retry-After = %q", ra)
	}
	// 400 ms de patience avec pauses 30, 60, 120 ms : 4 requetes ; la pause fixe de 30 ms en ferait ~13.
	if n := b.faux.Compteurs().Playlists; n > 5 {
		t.Errorf("%d requetes au fournisseur : martelage", n)
	}
	// Le diagnostic coute UNE lecture de user_info, quel que soit le nombre de refus.
	if n := b.userInfo(); n != 1 {
		t.Errorf("user_info lu %d fois, attendu 1", n)
	}
	// Retry-After croit a chaque refus consecutif de la meme (session, chaine) : 30, 60, 60.
	for _, attendu := range []string{"30", "60", "60"} {
		rec = b.get(PrefixeParDefaut+"/1001.m3u8", "s1")
		if ra := rec.Header().Get("Retry-After"); ra != attendu {
			t.Errorf("Retry-After = %q, attendu %q", ra, attendu)
		}
	}
	// Une autre chaine a sa propre serie.
	if ra := b.get(PrefixeParDefaut+"/1002.m3u8", "s1").Header().Get("Retry-After"); ra != "15" {
		t.Errorf("autre chaine : Retry-After = %q, attendu 15", ra)
	}
	if n := b.userInfo(); n != 1 {
		t.Errorf("user_info relu dans l'intervalle de cache : %d lectures", n)
	}

	b.faux.LibererOccupant()
	if rec = b.get(PrefixeParDefaut+"/1001.m3u8", "s1"); rec.Code != 200 {
		t.Fatalf("la lecture doit reprendre quand la place est rendue : %d", rec.Code)
	}
	// Un succes remet la serie a zero.
	b.faux.OccuperPar(time.Hour)
	if ra := b.get(PrefixeParDefaut+"/1001.m3u8", "s1").Header().Get("Retry-After"); ra != "15" {
		t.Errorf("apres un succes : Retry-After = %q, attendu 15", ra)
	}
	if b.refus.Load() != 0 {
		t.Error("un 407 ne doit pas declarer le compte refuse")
	}
	b.verifierSecrets(t)
}

// US-34 CA1 : controle du retrait. Sans lui (pause constante), le meme scenario martele.
func TestCA1_SansRetraitProgressifLeFournisseurEstMartele(t *testing.T) {
	b := nouvelleOccupation(t, 1, false)
	b.faux.OccuperPar(time.Hour)
	b.get(PrefixeParDefaut+"/1001.m3u8", "s1")
	if n := b.faux.Compteurs().Playlists; n < 8 {
		t.Errorf("le temoin de pause constante devait faire au moins 8 requetes : %d", n)
	}
}

// US-34 CA4/CA5 : un occupant externe prend la place en pleine lecture. La coupure est
// detectee et journalisee une fois par episode, comptee, exposee ; la classification est
// « ailleurs » ; le compte n'est pas declare refuse.
func TestCA4_PriseDePlaceEnPleineLectureDetectee(t *testing.T) {
	b := nouvelleOccupation(t, 1, true)
	for i := 0; i < 3; i++ { // lecture stable : 3 playlists reussies
		if rec := b.get(PrefixeParDefaut+"/1001.m3u8", "s1"); rec.Code != 200 {
			t.Fatalf("lecture %d : %d", i, rec.Code)
		}
	}
	if e := b.suivi.Etat("iptva"); e.Occupation != "mytv" || e.DernierePrise != nil {
		t.Fatalf("avant la prise : %+v", e)
	}
	b.faux.OccuperPar(time.Hour)
	rec := b.get(PrefixeParDefaut+"/1001.m3u8", "s1")
	if rec.Code != http.StatusServiceUnavailable || b.code(t, rec) != "compte_utilise_ailleurs" {
		t.Fatalf("coupure : %d %s", rec.Code, rec.Body.String())
	}
	b.get(PrefixeParDefaut+"/1001.m3u8", "s1") // meme episode
	if n := strings.Count(b.journal.String(), "connexion reprise par un autre appareil"); n != 1 {
		t.Errorf("l'episode doit etre journalise exactement une fois : %d", n)
	}
	if !strings.Contains(b.journal.String(), "level=WARN msg=\"connexion reprise par un autre appareil\"") {
		t.Errorf("niveau WARN attendu : %s", b.journal.String())
	}
	if n := b.mesures.Total("live_prises_de_place_total", "fournisseur", "iptva"); n != 1 {
		t.Errorf("live_prises_de_place_total = %v", n)
	}
	e := b.suivi.Etat("iptva")
	if e.Occupation != "ailleurs" || e.Depuis == nil || e.DernierePrise == nil {
		t.Errorf("etat expose : %+v", e)
	}
	if b.refus.Load() != 0 {
		t.Error("la supervision ne doit pas marquer le compte « refuse »")
	}
	// La place revient : le premier succes clot l'episode.
	b.faux.LibererOccupant()
	if rec = b.get(PrefixeParDefaut+"/1001.m3u8", "s1"); rec.Code != 200 {
		t.Fatalf("reprise : %d", rec.Code)
	}
	if e = b.suivi.Etat("iptva"); e.Occupation != "mytv" || e.DernierePrise == nil {
		t.Errorf("apres la reprise : %+v", e)
	}
	b.verifierSecrets(t)
}

// US-34 CA5 : deux sessions de mytv. La seconde se heurte au bail de la premiere sans que
// le fournisseur soit sollicite : compte_utilise_par_mytv.
func TestCA5_DeuxSessionsMytvConcurrentesParMytv(t *testing.T) {
	b := nouvelleOccupation(t, 1, true)
	if rec := b.get(PrefixeParDefaut+"/1001.m3u8", "s1"); rec.Code != 200 {
		t.Fatalf("%d", rec.Code)
	}
	b.faux.RazCompteurs()
	rec := b.get(PrefixeParDefaut+"/1001.m3u8", "s2")
	if rec.Code != 503 || b.code(t, rec) != "compte_utilise_par_mytv" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if c := b.faux.Compteurs(); c.Playlists != 0 || c.API != 0 {
		t.Errorf("ni flux ni user_info pour un refus local : %+v", c)
	}
}

// US-34 CA3 : le fournisseur refuse (407) alors qu'une AUTRE session de mytv tient un bail
// (le compte accepte plus de places que le fournisseur) : par_mytv, sans lire user_info.
func TestCA3_BailDUneAutreSessionClasseParMytvSansLireUserInfo(t *testing.T) {
	b := nouvelleOccupation(t, 2, true)
	b.faux.UneSeuleConnexion(time.Hour)
	if rec := b.get(PrefixeParDefaut+"/1001.m3u8", "s1"); rec.Code != 200 {
		t.Fatalf("%d", rec.Code)
	}
	rec := b.get(PrefixeParDefaut+"/1002.m3u8", "s2")
	if rec.Code != 503 || b.code(t, rec) != "compte_utilise_par_mytv" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if n := b.userInfo(); n != 0 {
		t.Errorf("user_info lu %d fois pour une cause deja connue", n)
	}
}

// US-34 CA3/CA5 : user_info en panne -> compte_occupe (indetermine), une seule tentative.
func TestCA5_UserInfoEnPanneDonneCompteOccupe(t *testing.T) {
	b := nouvelleOccupation(t, 1, true)
	b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleAPI, Statut: 500})
	b.faux.OccuperPar(time.Hour)
	rec := b.get(PrefixeParDefaut+"/1001.m3u8", "s1")
	if rec.Code != 503 || b.code(t, rec) != "compte_occupe" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	b.get(PrefixeParDefaut+"/1001.m3u8", "s1")
	if n := b.userInfo(); n != 1 {
		t.Errorf("l'echec est mis en cache aussi : %d lectures de user_info, attendu 1", n)
	}
}

// US-34 CA3 : apres un zapping, le fournisseur garde la place de la chaine PRECEDENTE de
// mytv. C'est mytv qui l'occupe, pas un autre appareil : compte_occupe, et inutile de
// questionner le fournisseur.
func TestCA3_ZappingPlaceGardeeParLeFournisseurEstCompteOccupe(t *testing.T) {
	b := nouvelleOccupation(t, 1, true)
	b.faux.UneSeuleConnexion(time.Hour)
	if rec := b.get(PrefixeParDefaut+"/1001.m3u8", "s1"); rec.Code != 200 {
		t.Fatalf("%d", rec.Code)
	}
	rec := b.get(PrefixeParDefaut+"/1002.m3u8", "s1")
	if rec.Code != 503 || b.code(t, rec) != "compte_occupe" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if n := b.userInfo(); n != 0 {
		t.Errorf("user_info lu %d fois alors que la cause est mytv", n)
	}
	if n := b.mesures.Total("live_prises_de_place_total"); n != 0 {
		t.Errorf("un zapping n'est pas une prise de place : %v", n)
	}
}

// US-34 CA6 : sans 407, le diagnostic ne coute rien chez le fournisseur.
func TestCA6_SansRefusAucuneLectureDeUserInfo(t *testing.T) {
	b := nouvelleOccupation(t, 1, true)
	for i := 0; i < 5; i++ {
		if rec := b.get(PrefixeParDefaut+"/1001.m3u8", "s1"); rec.Code != 200 {
			t.Fatalf("%d", rec.Code)
		}
	}
	if c := b.faux.Compteurs(); c.API != 0 || c.Refus407 != 0 {
		t.Errorf("aucun appel d'API attendu : %+v", c)
	}
}

// US-34 CA2 : un refus d'occupation ne declenche aucun appel supplementaire au fournisseur
// par un autre chemin : ecrire les reponses 503 suivantes (Retry-After 30, 60...) ne
// demande a nouveau ni flux ni user_info au-dela de la patience de chaque requete.
func TestCA2_LeRefusNeDeclencheAucunAppelEnPlus(t *testing.T) {
	b := nouvelleOccupation(t, 1, true)
	b.faux.OccuperPar(time.Hour)
	b.get(PrefixeParDefaut+"/1001.m3u8", "s1")
	apres1 := b.faux.Compteurs()
	b.get(PrefixeParDefaut+"/1001.m3u8", "s1")
	apres2 := b.faux.Compteurs()
	// Chaque requete du client fait sa patience (<= 5 flux) ; le total reste borne et user_info n'est pas relu.
	if apres2.Playlists-apres1.Playlists > 5 || apres2.ParAction[""] != 1 {
		t.Errorf("appels en trop : %+v puis %+v", apres1, apres2)
	}
	// Aucun appel a l'API en dehors de user_info.
	if apres2.API != 1 {
		t.Errorf("appels d'API = %d, attendu 1 (user_info)", apres2.API)
	}
}

// cheminsDeSegments liste les chemins de segments d'une playlist servie par le relais.
func cheminsDeSegments(corps string) []string {
	var s []string
	for _, l := range strings.Split(corps, "\n") {
		if strings.HasPrefix(l, PrefixeParDefaut) {
			s = append(s, l)
		}
	}
	return s
}

// US-34 CA4 : c'est sur les SEGMENTS que la coupure du 2026-10-16 est apparue (407 puis
// 401/403 apres une lecture stable). Ici : succes de segments distincts, puis le 407 d'un
// occupant externe -> prise de place detectee, classification « ailleurs ».
func TestCA4_PriseDePlaceVueSurUnSegment(t *testing.T) {
	b := nouvelleOccupation(t, 1, true)
	pl := b.get(PrefixeParDefaut+"/1001.m3u8", "s1")
	segs := cheminsDeSegments(pl.Body.String())
	if len(segs) < 5 {
		t.Fatalf("playlist inattendue : %s", pl.Body.String())
	}
	for _, s := range segs[:3] {
		if rec := b.get(s, "s1"); rec.Code != 200 {
			t.Fatalf("segment : %d", rec.Code)
		}
	}
	b.faux.OccuperPar(time.Hour)
	rec := b.get(segs[3], "s1")
	if rec.Code != 503 || b.code(t, rec) != "compte_utilise_ailleurs" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if n := b.mesures.Total("live_prises_de_place_total"); n != 1 {
		t.Errorf("prises de place = %v", n)
	}
	if e := b.suivi.Etat("iptva"); e.Occupation != "ailleurs" || e.DernierePrise == nil {
		t.Errorf("etat : %+v", e)
	}
}

// US-34 CA4 : un 403 de segment apres des succes est aussi la trace d'une prise de place,
// mais reste `segment_expire` pour le client (lot 4) et ne declare PAS le compte refuse.
func TestCA4_Segment403ApresSuccesEstUnePriseMaisResteSegmentExpire(t *testing.T) {
	b := nouvelleOccupation(t, 1, true)
	segs := cheminsDeSegments(b.get(PrefixeParDefaut+"/1001.m3u8", "s1").Body.String())
	for _, s := range segs[:3] {
		if rec := b.get(s, "s1"); rec.Code != 200 {
			t.Fatalf("segment : %d", rec.Code)
		}
	}
	b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleSegment, Statut: 403, Fois: 1})
	rec := b.get(segs[3], "s1")
	if rec.Code != http.StatusBadGateway || b.code(t, rec) != "segment_expire" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if n := b.mesures.Total("live_prises_de_place_total"); n != 1 {
		t.Errorf("prises de place = %v", n)
	}
	if b.refus.Load() != 0 {
		t.Error("le 403 d'un segment ne declare pas le compte refuse")
	}
}

// US-34 CA4 : un segment perime sans lecture recente (lecteur en pause) n'est pas une prise de place.
func TestCA4_Segment403SansSuccesRecentNEstPasUnePrise(t *testing.T) {
	b := nouvelleOccupation(t, 1, true)
	segs := cheminsDeSegments(b.get(PrefixeParDefaut+"/1001.m3u8", "s1").Body.String())
	b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleSegment, Statut: 403, Fois: 1})
	b.get(segs[0], "s1")
	if n := b.mesures.Total("live_prises_de_place_total"); n != 0 {
		t.Errorf("prises de place = %v", n)
	}
	if strings.Contains(b.journal.String(), "reprise par un autre appareil") {
		t.Error("aucune alerte attendue")
	}
}
