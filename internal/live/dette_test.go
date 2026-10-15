package live

import (
	"context"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mytv/internal/config"
)

// ---- Dette lot 1 : /etat reserve aux administrateurs ----

func TestEtatReserveAuxAdministrateurs(t *testing.T) {
	b := nouveauBanc(t, amontSimple(nil), nil)
	if rec := b.get(PrefixeParDefaut+"/etat", "s1"); rec.Code != http.StatusForbidden {
		t.Errorf("utilisateur ordinaire : %d", rec.Code)
	}
	if rec := b.get(PrefixeParDefaut+"/etat", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonyme : %d", rec.Code)
	}
	if rec := b.get(PrefixeParDefaut+"/etat", "s1", "X-Admin", "1"); rec.Code != http.StatusOK {
		t.Errorf("administrateur : %d", rec.Code)
	}
	// Refus par defaut : sans EstAdmin, personne n'y accede.
	b = nouveauBanc(t, amontSimple(nil), func(o *Options) { o.EstAdmin = nil })
	if rec := b.get(PrefixeParDefaut+"/etat", "s1", "X-Admin", "1"); rec.Code != http.StatusForbidden {
		t.Errorf("sans EstAdmin : %d", rec.Code)
	}
}

// ---- Dette lot 1 : jeton de segment lie a la chaine du chemin ----

func TestJetonDeSegmentRefuseSurUneAutreChaine(t *testing.T) {
	var segs atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/live/", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, playlistAmont("aaaa")) })
	mux.HandleFunc("/hls/", func(w http.ResponseWriter, r *http.Request) { segs.Add(1); corpsDe(w, 5) })
	b := nouveauBanc(t, mux, nil)
	seg := chaineSeg(t, b, "100")[0]
	if rec := b.segment(seg, "s1"); rec.Code != 200 {
		t.Fatalf("chaine d'origine : %d", rec.Code)
	}
	avant, baux := segs.Load(), b.baux.nbAcquisitions()
	autre := strings.Replace(seg, "/100/seg/", "/200/seg/", 1)
	if autre == seg {
		t.Fatalf("chemin inattendu : %s", seg)
	}
	if rec := b.segment(autre, "s1"); rec.Code != http.StatusForbidden {
		t.Errorf("jeton d'une autre chaine : %d (403 attendu)", rec.Code)
	}
	if segs.Load() != avant || b.baux.nbAcquisitions() != baux {
		t.Error("un jeton refuse ne doit ni toucher le fournisseur ni prendre de bail")
	}
}

// ---- Dette lot 1 : filtre d'hotes (SSRF) ----

func playlistVers(url string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "#EXTM3U\n#EXTINF:1,\n"+url+"\n")
	})
}

func TestSegmentVersAdressePriveeRefuse(t *testing.T) {
	for _, cible := range []string{
		"http://169.254.169.254/latest/meta-data/", "http://10.0.0.5/x.ts", "http://192.168.1.10/x.ts",
		"http://172.16.0.1/x.ts", "http://127.0.0.2:9999/x.ts", "http://[::1]/x.ts", "http://0.0.0.0/x.ts",
		"http://100.64.0.1/x.ts", "http://[fe80::1]/x.ts", "http://[::ffff:10.0.0.1]/x.ts", "http://interne.test/x.ts",
	} {
		b := nouveauBanc(t, playlistVers(cible), func(o *Options) {
			o.ResoudreHote = func(_ context.Context, h string) ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr("10.9.9.9")}, nil
			}
		})
		seg := chaineSeg(t, b, "100")[0]
		if rec := b.segment(seg, "s1"); rec.Code != http.StatusForbidden {
			t.Errorf("%s : %d (403 attendu)", cible, rec.Code)
		}
	}
}

func TestSegmentVersHotePublicOuAutorise(t *testing.T) {
	resolveur := func(_ context.Context, h string) ([]netip.Addr, error) {
		if h == "cdn.exemple.test" {
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("192.168.50.7")}, nil
	}
	// Public : le filtre laisse passer (la connexion echouera ensuite, peu importe : pas 403).
	b := nouveauBanc(t, playlistVers("http://cdn.exemple.test/x.ts"), func(o *Options) { o.ResoudreHote = resolveur })
	if rec := b.segment(chaineSeg(t, b, "100")[0], "s1"); rec.Code == http.StatusForbidden {
		t.Error("hote public refuse")
	}
	// LAN : refuse par defaut, accepte si la config le liste (nom, adresse ou plage).
	for _, autorise := range [][]string{nil, {"lan.test"}, {"192.168.50.0/24"}} {
		b = nouveauBanc(t, playlistVers("http://lan.test/x.ts"), func(o *Options) { o.ResoudreHote = resolveur; o.HotesAutorises = autorise })
		rec := b.segment(chaineSeg(t, b, "100")[0], "s1")
		if (autorise == nil) != (rec.Code == http.StatusForbidden) {
			t.Errorf("autorises %v : %d", autorise, rec.Code)
		}
	}
	// Un nom qui resout vers PLUSIEURS adresses est refuse si l'une est interne.
	b = nouveauBanc(t, playlistVers("http://double.test/x.ts"), func(o *Options) {
		o.ResoudreHote = func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("93.184.216.34"), netip.MustParseAddr("127.0.0.1")}, nil
		}
	})
	if rec := b.segment(chaineSeg(t, b, "100")[0], "s1"); rec.Code != http.StatusForbidden {
		t.Errorf("resolution mixte : %d", rec.Code)
	}
	// Entree de configuration invalide : refusee au demarrage.
	l, ba, n := reglages()
	if _, err := Nouveau(Options{Fournisseur: &fournisseurFactice{}, Baux: &bauxFactice{}, Live: l, Bail: ba, Reseau: n, Secret: secretTest, HotesAutorises: []string{"pas valide/"}}); err == nil {
		t.Error("hote autorise invalide accepte")
	}
}

// L'hote du fournisseur lui-meme n'est jamais filtre (il peut etre sur le LAN en test).
func TestHoteDuFournisseurToujoursAutorise(t *testing.T) {
	b := nouveauBanc(t, amontSimple(nil), nil) // fournisseur sur 127.0.0.1
	if rec := b.segment(chaineSeg(t, b, "100")[0], "s1"); rec.Code != 200 {
		t.Fatalf("%d", rec.Code)
	}
}

// ---- Dette lot 1 : la patience du 407 n'ajoute pas celle des baux ----

func TestPatience407DeduiteDeLAttenteDuBail(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/live/", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "place occupee", 407) })
	b := nouveauBanc(t, mux, func(o *Options) { o.Bail.PatienceMax = config.Duree(500 * time.Millisecond) })
	b.baux.surAcquerir = func() { time.Sleep(350 * time.Millisecond) } // Acquerir a deja patiente 350 ms
	debut := time.Now()
	rec := b.playlist("100", "s1")
	d := time.Since(debut)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("503 attendu : %d", rec.Code)
	}
	// Cumulee, la requete durerait ~850 ms ; deduite, ~500 ms au total.
	if d > 700*time.Millisecond {
		t.Errorf("patience cumulee avec celle du bail : %v (borne totale 500 ms)", d)
	}
}

// ---- Dette lot 1 : constantes de portee locale -> configuration ----

func TestReglagesTechniquesObligatoires(t *testing.T) {
	l, ba, n := reglages()
	base := Options{Fournisseur: &fournisseurFactice{}, Baux: &bauxFactice{}, Live: l, Bail: ba, Reseau: n, Secret: secretTest}
	for nom, f := range map[string]func(*Options){
		"taille_playlist_max": func(o *Options) { o.Live.TaillePlaylistMax = 0 },
		"taille_tampon":       func(o *Options) { o.Live.TailleTampon = 0 },
		"fenetre_mesures":     func(o *Options) { o.Live.FenetreMesures = 0 },
		"part_max_segment":    func(o *Options) { o.Live.PartMaxSegment = 0 },
	} {
		o := base
		f(&o)
		if _, err := Nouveau(o); err == nil {
			t.Errorf("%s nul accepte", nom)
		}
	}
}

func TestReglagesTechniquesAppliques(t *testing.T) {
	// taille_playlist_max : une playlist plus grande que la borne est tronquee (donc refusee).
	grosse := "#EXTM3U\n" + strings.Repeat("#EXT-X-COMMENT:xxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\n", 100) + "#EXTINF:1,\ns.ts\n"
	mux := http.NewServeMux()
	mux.HandleFunc("/live/", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, grosse) })
	b := nouveauBanc(t, mux, func(o *Options) { o.Live.TaillePlaylistMax = 200 })
	if rec := b.playlist("100", "s1"); strings.Contains(rec.Body.String(), "s.ts") {
		t.Error("la borne live.taille_playlist_max n'est pas appliquee")
	}
	// fenetre_mesures : les mesures gardees sont bornees par la config.
	b = nouveauBanc(t, amontSimple(nil), func(o *Options) { o.Live.FenetreMesures = 3 })
	for i := 0; i < 10; i++ {
		b.relais.st.noterSegment(time.Millisecond)
	}
	if n := len(b.relais.st.segments); n != 3 {
		t.Errorf("fenetre_mesures non appliquee : %d mesures", n)
	}
}
