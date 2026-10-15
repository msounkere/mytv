package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

const minimal = `
serveur: { secret_signature: { env: T_SECRET } }
fournisseurs:
  - { id: a, type: xtream, base: "http://f.test", utilisateur: { env: T_U }, mot_de_passe: { env: T_P } }
`

func lire(t *testing.T, s string) (*Config, error) {
	t.Helper()
	return Lire(strings.NewReader(s))
}

func TestDefautsAppliques(t *testing.T) {
	c, err := lire(t, minimal)
	if err != nil {
		t.Fatal(err)
	}
	f := c.Fournisseurs[0]
	if f.ConnexionsMax != 1 || f.Reseau.EssaisPlaylist != 3 || f.Rafraichissement.Catalogue.Std() != 24*time.Hour {
		t.Errorf("defauts du fournisseur non appliques : %+v", f)
	}
	if c.Bail.PatienceMax.Std() != 2*time.Minute || c.Live.PeremptionSecours.Std() != 75*time.Second {
		t.Errorf("defauts globaux non appliques")
	}
}

func TestCleInconnueRefusee(t *testing.T) {
	_, err := lire(t, minimal+"\nlive: { fraicheur_playlsit: 1s }\n")
	if err == nil || !strings.Contains(err.Error(), "fraicheur_playlsit") {
		t.Fatalf("une faute de frappe doit etre refusee : %v", err)
	}
}

func TestDureeInvalide(t *testing.T) {
	_, err := lire(t, minimal+"\nbail: { ttl: \"90 secondes\" }\n")
	if err == nil || !strings.Contains(err.Error(), "invalide") {
		t.Fatalf("duree invalide non signalee : %v", err)
	}
}

func TestValidationRassembleLesErreurs(t *testing.T) {
	_, err := lire(t, `
fournisseurs:
  - { id: "a:b", type: m3u, base: "line.test" }
  - { id: "a:b", type: xtream, base: "http://x", utilisateur: { env: U }, mot_de_passe: { env: P }, connexions_max: -1 }
journal: { niveau: bavard }
`)
	if err == nil {
		t.Fatal("erreur attendue")
	}
	for _, attendu := range []string{"secret_signature", "ne doit pas contenir", "type \"m3u\"", "http://", "en double", "connexions_max", "niveau"} {
		if !strings.Contains(err.Error(), attendu) {
			t.Errorf("l'erreur devrait mentionner %q :\n%v", attendu, err)
		}
	}
}

func TestSecretEnvEtFichier(t *testing.T) {
	t.Setenv("T_X", "valeur")
	if v, err := (Secret{Env: "T_X"}).Valeur(); err != nil || v != "valeur" {
		t.Fatalf("env : %q %v", v, err)
	}
	f := t.TempDir() + "/s"
	os.WriteFile(f, []byte("depuis-fichier\n"), 0o600)
	if v, err := (Secret{Fichier: f}).Valeur(); err != nil || v != "depuis-fichier" {
		t.Fatalf("fichier : %q %v", v, err)
	}
	if _, err := (Secret{Env: "T_X", Fichier: f}).Valeur(); err == nil {
		t.Error("env et fichier ensemble doivent etre refuses")
	}
	if _, err := (Secret{Env: "T_ABSENTE"}).Valeur(); err == nil {
		t.Error("variable absente doit etre refusee")
	}
}

func TestResoudreEchoueTotDemarrage(t *testing.T) {
	c, err := lire(t, minimal)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Resoudre(); err == nil || !strings.Contains(err.Error(), "T_SECRET") {
		t.Fatalf("secrets manquants non detectes : %v", err)
	}
	t.Setenv("T_SECRET", "s")
	t.Setenv("T_U", "u")
	t.Setenv("T_P", "p")
	if err := c.Resoudre(); err != nil {
		t.Fatalf("secrets presents : %v", err)
	}
}

func TestFichierExempleValide(t *testing.T) {
	c, err := Charger("../../config/mytv.exemple.yaml")
	if err != nil {
		t.Fatalf("l'exemple fourni doit etre valide : %v", err)
	}
	if c.Fournisseurs[0].ID != "iptva" {
		t.Errorf("exemple inattendu : %+v", c.Fournisseurs[0])
	}
}

// US-08 : les reglages de comptes dangereux sont refuses, avec toutes les erreurs.
func TestComptesReglagesDangereuxRefuses(t *testing.T) {
	_, err := lire(t, minimal+`
comptes:
  argon2: { memoire_kio: 1024, iterations: 1, parallelisme: 1 }
  mot_de_passe_min: 4
  tentatives: { seuil: 0, fenetre: 0s }
  plages_locales: ["pas une plage"]
  proxys_fiables: ["300.1.1.1"]
  local_utilisateur: local
`)
	if err == nil {
		t.Fatal("reglages dangereux acceptes")
	}
	for _, m := range []string{"argon2 trop faible", "mot_de_passe_min", "tentatives", "plages_locales", "proxys_fiables"} {
		if !strings.Contains(err.Error(), m) {
			t.Errorf("erreur %q absente de : %v", m, err)
		}
	}
	_, err = lire(t, minimal+"\ncomptes: { local_utilisateur: local }\n")
	if err == nil || !strings.Contains(err.Error(), "plages_locales") {
		t.Fatalf("compte local sans plage : %v", err)
	}
	if _, err = lire(t, minimal+"\ncomptes: { local_utilisateur: local, plages_locales: [192.168.50.0/24, 10.0.0.1], proxys_fiables: ['::1'] }\n"); err != nil {
		t.Fatalf("reglages valides refuses : %v", err)
	}
}

func TestEPGDefautsEtValidation(t *testing.T) {
	c, err := lire(t, minimal)
	if err != nil {
		t.Fatal(err)
	}
	if c.EPG.Futur.Std() != 7*24*time.Hour || c.EPG.Fuseau != "UTC" || c.EPG.TailleMax <= 0 {
		t.Fatalf("defauts epg inattendus : %+v", c.EPG)
	}
	_, err = lire(t, minimal+"epg: { futur: 0s, fuseau: Mars/Olympus, taille_max: -1 }\n")
	if err == nil {
		t.Fatal("epg invalide accepte")
	}
	for _, attendu := range []string{"epg.futur", "epg.taille_max", "epg.fuseau"} {
		if !strings.Contains(err.Error(), attendu) {
			t.Errorf("l'erreur devrait mentionner %q :\n%v", attendu, err)
		}
	}
}

// US-11 : reglages de l'API, du direct et du filtre d'hotes valides au chargement.
func TestReglagesAPIEtLiveRefuses(t *testing.T) {
	_, err := lire(t, `
fournisseurs:
  - { id: a, type: xtream, base: "http://f.test", utilisateur: { env: T_U }, mot_de_passe: { env: T_P } }
api: { corps_max: 0, pagination_max: 10, pagination_defaut: 50, origines_cors: ["*", "https://ok.example", "https://x.example/"] }
live: { taille_tampon: 0 }
serveur: { secret_signature: { env: T_SECRET }, hotes_segments_autorises: ["a b"] }
`)
	if err == nil {
		t.Fatal("reglages invalides acceptes")
	}
	for _, m := range []string{"api.corps_max", "pagination_defaut", "origines_cors", "live.taille_playlist_max", "hotes_segments_autorises"} {
		if !strings.Contains(err.Error(), m) {
			t.Errorf("%q absent de : %v", m, err)
		}
	}
	if strings.Count(err.Error(), "origines_cors") != 2 {
		t.Errorf("« * » et l'origine avec « / » final doivent etre refuses, pas l'origine valide : %v", err)
	}
}

func TestServeurDelaiArretEtUI(t *testing.T) {
	c, err := lire(t, minimal)
	if err != nil || c.Serveur.DelaiArret.Std() != 20*time.Second || c.Serveur.UIDossier != "" {
		t.Fatalf("defauts : %+v %v", c.Serveur, err)
	}
	if _, err := lire(t, `
serveur: { secret_signature: { env: T_SECRET }, delai_arret: 0s, ui_dossier: /srv/ui }
fournisseurs:
  - { id: a, type: xtream, base: "http://f.test", utilisateur: { env: T_U }, mot_de_passe: { env: T_P } }
`); err == nil || !strings.Contains(err.Error(), "delai_arret") {
		t.Fatalf("delai_arret nul accepte : %v", err)
	}
}

// US-16 : api.delai_export (export en bloc du catalogue) a un defaut et est valide.
func TestDelaiExportDefautEtValidation(t *testing.T) {
	c, err := lire(t, minimal)
	if err != nil || c.API.DelaiExport.Std() != 5*time.Minute {
		t.Fatalf("defaut : %v %v", c.API.DelaiExport, err)
	}
	_, err = lire(t, `
serveur: { secret_signature: { env: T_SECRET } }
fournisseurs:
  - { id: a, type: xtream, base: "http://f.test", utilisateur: { env: T_U }, mot_de_passe: { env: T_P } }
api: { delai_export: 0s }
`)
	if err == nil || !strings.Contains(err.Error(), "api.delai_export") {
		t.Fatalf("delai_export nul accepte : %v", err)
	}
}

// US-17 CA3 : vod.pause_max a un defaut (5 min), peut valoir 0 (jamais) et refuse le negatif.
func TestVodPauseMax(t *testing.T) {
	c, err := lire(t, minimal)
	if err != nil || c.Vod.PauseMax.Std() != 5*time.Minute {
		t.Fatalf("defaut : %v %v", c.Vod.PauseMax, err)
	}
	c, err = lire(t, minimal+"\nvod: { pause_max: 90s }\n")
	if err != nil || c.Vod.PauseMax.Std() != 90*time.Second {
		t.Fatalf("90s : %v %v", c.Vod.PauseMax, err)
	}
	c, err = lire(t, minimal+"\nvod: { pause_max: 0s }\n")
	if err != nil || c.Vod.PauseMax != 0 {
		t.Fatalf("0s (jamais) : %v %v", c.Vod.PauseMax, err)
	}
	if _, err = lire(t, minimal+"\nvod: { pause_max: -5s }\n"); err == nil {
		t.Error("pause_max negatif accepte")
	}
	if _, err = lire(t, minimal+"\nvod: { pause: 5s }\n"); err == nil {
		t.Error("cle inconnue acceptee")
	}
}

// US-24 CA4 : vod.relais_brut_hevc est faux par defaut (a n'activer que sur demande), accepte
// un booleen et refuse toute autre valeur.
func TestVodRelaisBrutHEVC(t *testing.T) {
	c, err := lire(t, minimal)
	if err != nil || c.Vod.RelaisBrutHEVC {
		t.Fatalf("defaut : %v %v", c.Vod.RelaisBrutHEVC, err)
	}
	c, err = lire(t, minimal+"\nvod: { relais_brut_hevc: true }\n")
	if err != nil || !c.Vod.RelaisBrutHEVC {
		t.Fatalf("true : %v %v", c.Vod.RelaisBrutHEVC, err)
	}
	if _, err = lire(t, minimal+"\nvod: { relais_brut_hevc: peut-etre }\n"); err == nil {
		t.Error("valeur non booleenne acceptee")
	}
}

// US-23 CA4 : defauts du proxy d'images et validation de chaque reglage.
func TestImagesDefautsEtValidation(t *testing.T) {
	c, err := lire(t, minimal)
	if err != nil {
		t.Fatal(err)
	}
	if c.Images.CacheOctets <= 0 || c.Images.TailleMax <= 0 || c.Images.ConcurrenceMax < 1 || c.Images.Delai <= 0 || c.Images.CacheNegatif <= 0 {
		t.Errorf("defauts de images non appliques : %+v", c.Images)
	}
	refuses := map[string]string{
		"images: { cache_dossier: \"\" }":                       "images.cache_dossier",
		"images: { taille_max: 0 }":                             "images.taille_max",
		"images: { taille_max: 1000, cache_octets: 999 }":       "images.cache_octets",
		"images: { delai: 0s }":                                 "images.delai",
		"images: { cache_validite: 0s }":                        "images.cache_validite",
		"images: { concurrence_max: 0 }":                        "images.concurrence_max",
		"images: { cache_negatif_max: 0 }":                      "images.cache_negatif_max",
		"images: { redirections_max: 11 }":                      "images.redirections_max",
		"images: { redirections_max: -1 }":                      "images.redirections_max",
		"images: { cache_negatif: 1s, inconnu: 3 }":             "inconnu",
		"images: { delai: abc }":                                "duree",
		"images: { cache_octets: 10, taille_max: 20 }":          "images.cache_octets",
		"images: { cache_dossier: x, concurrence_max: -3 }":     "images.concurrence_max",
		"images: { cache_negatif: 0s, cache_negatif_max: 100 }": "", // 0 = pas de cache negatif : valide
	}
	for yaml, attendu := range refuses {
		_, err := lire(t, minimal+"\n"+yaml+"\n")
		if attendu == "" {
			if err != nil {
				t.Errorf("%s : doit etre valide : %v", yaml, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), attendu) {
			t.Errorf("%s : doit etre refuse en citant %q, obtenu %v", yaml, attendu, err)
		}
	}
}

// US-25 CA2/CA4 : valeurs par defaut et validation de supervision.*.
func TestSupervisionDefautsEtValidation(t *testing.T) {
	c, err := lire(t, minimal)
	if err != nil {
		t.Fatal(err)
	}
	s := c.Supervision
	if s.AlerteExpiration.Std() != 7*24*time.Hour || s.EchecsSynchro != 3 || s.Webhook.URL != "" {
		t.Errorf("defauts : %+v", s)
	}
	cas := []struct{ yaml, attendu string }{
		{"supervision: { alerte_expiration: 0s }", "alerte_expiration"},
		{"supervision: { echecs_synchro: 0 }", "echecs_synchro"},
		{"supervision: { periode_alerte: 0s }", "periode_alerte"},
		{"supervision: { webhook: { url: \"http://h.test/x\" } }", "https"},
		{"supervision: { webhook: { url: \"https://u:p@h.test/x\" } }", "identifiants"},
		{"supervision: { webhook: { url: \"https:///x\" } }", "adresse valide"},
		{"supervision: { webhook: { url: \"https://h.test/x\", delai: 0s } }", "webhook.delai"},
		{"supervision: { webhook: { secret: { env: X } } }", "sans supervision.webhook.url"},
	}
	for _, k := range cas {
		_, err := lire(t, minimal+"\n"+k.yaml+"\n")
		if err == nil || !strings.Contains(err.Error(), k.attendu) {
			t.Errorf("%s : attendu %q, obtenu %v", k.yaml, k.attendu, err)
		}
	}
	// Le jeton que porte l'URL n'apparait jamais dans le message d'erreur.
	_, err = lire(t, minimal+"\nsupervision: { webhook: { url: \"http://h.test/hook/JETON-SECRET-42\" } }\n")
	if err == nil || strings.Contains(err.Error(), "JETON-SECRET-42") {
		t.Errorf("l'erreur ne doit pas citer l'URL : %v", err)
	}
	if _, err := lire(t, minimal+"\nsupervision: { webhook: { url: \"https://h.test/x\", secret: { env: T_W } } }\n"); err != nil {
		t.Errorf("webhook valide refuse : %v", err)
	}
}

// US-25 CA4 : le secret du webhook est resolu des le demarrage (Resoudre).
func TestResoudreVerifieLeSecretDuWebhook(t *testing.T) {
	t.Setenv("T_SECRET", "x")
	t.Setenv("T_U", "u")
	t.Setenv("T_P", "p")
	c, err := lire(t, minimal+"\nsupervision: { webhook: { url: \"https://h.test/x\", secret: { env: T_W_ABSENT } } }\n")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Resoudre(); err == nil || !strings.Contains(err.Error(), "supervision.webhook.secret") {
		t.Errorf("secret absent non signale : %v", err)
	}
}

// US-27 CA1 : reseau.fermer_connexions_flux vaut vrai quand il n'est pas renseigne, accepte
// faux, et refuse une valeur qui n'est pas un booleen.
func TestReseauFermerConnexionsFlux(t *testing.T) {
	c, err := lire(t, minimal)
	if err != nil || !c.Fournisseurs[0].Reseau.FermeLesFlux() {
		t.Fatalf("defaut : %v", err)
	}
	avec := func(v string) string {
		return strings.Replace(minimal, "mot_de_passe: { env: T_P } }", "mot_de_passe: { env: T_P }, reseau: { fermer_connexions_flux: "+v+" } }", 1)
	}
	c, err = lire(t, avec("false"))
	if err != nil || c.Fournisseurs[0].Reseau.FermeLesFlux() {
		t.Fatalf("faux explicite : %v", err)
	}
	c, err = lire(t, avec("true"))
	if err != nil || !c.Fournisseurs[0].Reseau.FermeLesFlux() {
		t.Fatalf("vrai explicite : %v", err)
	}
	if _, err = lire(t, avec("souvent")); err == nil {
		t.Error("valeur non booleenne acceptee")
	}
	if !(Reseau{}).FermeLesFlux() {
		t.Error("valeur nulle : doit fermer (config ecrite avant l'option)")
	}
}
