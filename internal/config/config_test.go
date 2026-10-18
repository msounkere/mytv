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
	if c.Bail.PatienceMax.Std() != 2*time.Minute || c.Bail.PatienceSegment.Std() != 8*time.Second || c.Live.PeremptionSecours.Std() != 75*time.Second {
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

// vod.grace_annulation : 250 ms par defaut, reglable, jamais nulle ni negative.
func TestVodGraceAnnulation(t *testing.T) {
	c, err := lire(t, minimal)
	if err != nil || c.Vod.GraceAnnulation.Std() != 250*time.Millisecond {
		t.Fatalf("defaut : %v %v", c.Vod.GraceAnnulation, err)
	}
	c, err = lire(t, minimal+"\nvod: { grace_annulation: 100ms }\n")
	if err != nil || c.Vod.GraceAnnulation.Std() != 100*time.Millisecond {
		t.Fatalf("100ms : %v %v", c.Vod.GraceAnnulation, err)
	}
	if _, err = lire(t, minimal+"\nvod: { grace_annulation: 0s }\n"); err == nil || !strings.Contains(err.Error(), "vod.grace_annulation") {
		t.Errorf("grace nulle acceptee : %v", err)
	}
	if _, err = lire(t, minimal+"\nvod: { grace_annulation: -1s }\n"); err == nil {
		t.Error("grace negative acceptee")
	}
}

// vod.retention_adresse : 10 min par defaut, reglable, jamais nulle.
func TestVodRetentionAdresse(t *testing.T) {
	c, err := lire(t, minimal)
	if err != nil || c.Vod.RetentionAdresse.Std() != 10*time.Minute {
		t.Fatalf("defaut : %v %v", c.Vod.RetentionAdresse, err)
	}
	c, err = lire(t, minimal+"\nvod: { retention_adresse: 2m }\n")
	if err != nil || c.Vod.RetentionAdresse.Std() != 2*time.Minute {
		t.Fatalf("2m : %v %v", c.Vod.RetentionAdresse, err)
	}
	if _, err = lire(t, minimal+"\nvod: { retention_adresse: 0s }\n"); err == nil || !strings.Contains(err.Error(), "vod.retention_adresse") {
		t.Errorf("retention nulle acceptee : %v", err)
	}
}

// vod.premier_octet : 60 s par defaut, reglable, jamais nul (un ffmpeg muet doit finir en 502).
func TestVodPremierOctet(t *testing.T) {
	c, err := lire(t, minimal)
	if err != nil || c.Vod.PremierOctet.Std() != 60*time.Second {
		t.Fatalf("defaut : %v %v", c.Vod.PremierOctet, err)
	}
	c, err = lire(t, minimal+"\nvod: { premier_octet: 20s }\n")
	if err != nil || c.Vod.PremierOctet.Std() != 20*time.Second {
		t.Fatalf("20s : %v %v", c.Vod.PremierOctet, err)
	}
	if _, err = lire(t, minimal+"\nvod: { premier_octet: 0s }\n"); err == nil || !strings.Contains(err.Error(), "vod.premier_octet") {
		t.Errorf("premier_octet nul accepte : %v", err)
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

// US-26 CA1 : sauvegarde.* a des defauts et une validation.
func TestSauvegardeDefautsEtValidation(t *testing.T) {
	c, err := lire(t, minimal)
	if err != nil {
		t.Fatal(err)
	}
	if c.Sauvegarde.Heure != "03:30" || c.Sauvegarde.Conservation != 14 {
		t.Errorf("defauts : %+v", c.Sauvegarde)
	}
	// Dossier par defaut : a cote de la base.
	c.Stockage.Fichier = "/var/lib/mytv/mytv.db"
	if d := c.DossierSauvegardes(); d != "/var/lib/mytv/sauvegardes" {
		t.Errorf("dossier par defaut : %s", d)
	}
	c.Sauvegarde.Dossier = "/mnt/nas/mytv"
	if d := c.DossierSauvegardes(); d != "/mnt/nas/mytv" {
		t.Errorf("dossier configure : %s", d)
	}
	for yaml, attendu := range map[string]string{
		"sauvegarde: { heure: \"25:99\" }": "sauvegarde.heure",
		"sauvegarde: { heure: \"3h30\" }":  "sauvegarde.heure",
		"sauvegarde: { conservation: 0 }":  "sauvegarde.conservation",
		"sauvegarde: { conservation: -3 }": "sauvegarde.conservation",
		"sauvegarde: { dosier: /x }":       "dosier", // faute de frappe
	} {
		if _, err := lire(t, minimal+"\n"+yaml+"\n"); err == nil || !strings.Contains(err.Error(), attendu) {
			t.Errorf("%s : attendu %q, obtenu %v", yaml, attendu, err)
		}
	}
	// Heure vide = planification desactivee, acceptee.
	if c, err := lire(t, minimal+"\nsauvegarde: { heure: \"\" }\n"); err != nil || c.Sauvegarde.Heure != "" {
		t.Errorf("heure vide : %v", err)
	}
}

// US-26 CA3 : serveur.ecoute est valide (adresse:port), et l'ecoute sur toutes les interfaces
// sans proxy ni plage locale est signalee SANS etre interdite.
func TestEcouteValideeEtAvertissement(t *testing.T) {
	avec := func(ecoute string) string {
		return strings.Replace(minimal, "serveur: { secret_signature", "serveur: { ecoute: \""+ecoute+"\", secret_signature", 1)
	}
	for _, mauvais := range []string{"8080", "127.0.0.1", "127.0.0.1:http-alt", "127.0.0.1:99999", "[::1:8080"} {
		if _, err := lire(t, avec(mauvais)); err == nil || !strings.Contains(err.Error(), "serveur.ecoute") {
			t.Errorf("%q accepte ou erreur sans serveur.ecoute : %v", mauvais, err)
		}
	}
	cas := []struct {
		ecoute, extra string
		avertit       bool
	}{
		{"0.0.0.0:8080", "", true},
		{":8080", "", true},
		{"[::]:8080", "", true},
		{"0.0.0.0:8080", "comptes: { proxys_fiables: [\"10.0.0.1\"] }", false},
		{"0.0.0.0:8080", "comptes: { plages_locales: [\"192.168.50.0/24\"] }", false},
		{"127.0.0.1:8080", "", false},
		{"192.168.50.3:8080", "", false},
	}
	for _, k := range cas {
		c, err := lire(t, avec(k.ecoute)+"\n"+k.extra+"\n")
		if err != nil {
			t.Errorf("%s : l'option doit rester possible : %v", k.ecoute, err)
			continue
		}
		a := c.Avertissements()
		if (len(a) > 0) != k.avertit {
			t.Errorf("%s %s : avertissements %v, attendu %v", k.ecoute, k.extra, a, k.avertit)
		}
		if k.avertit && !strings.Contains(a[0], "proxys_fiables") {
			t.Errorf("l'avertissement doit dire quoi faire : %s", a[0])
		}
	}
}

// US-28 CA2/CA4 : reglages des metadonnees, defauts sensés et validation.
func TestMetadonneesDefautsEtValidation(t *testing.T) {
	c, err := lire(t, minimal)
	m := c.Metadonnees
	if err != nil || !m.Actif || m.DureeVie.Std() != 30*24*time.Hour || m.ConcurrenceMax != 2 || m.AppelsParMinute != 30 || m.Delai <= 0 || m.CoupeCircuit <= 0 {
		t.Fatalf("defauts : %+v %v", m, err)
	}
	c, err = lire(t, minimal+"\nmetadonnees: { duree_vie: 48h, concurrence_max: 4, appels_par_minute: 10, actif: false }\n")
	if err != nil || c.Metadonnees.Actif || c.Metadonnees.DureeVie.Std() != 48*time.Hour || c.Metadonnees.ConcurrenceMax != 4 {
		t.Fatalf("surcharge : %+v %v", c.Metadonnees, err)
	}
	for _, mauvais := range []string{"duree_vie: 0s", "delai: 0s", "delai: 10m", "coupe_circuit: 0s", "concurrence_max: 0",
		"concurrence_max: 99", "appels_par_minute: 0", "appels_par_minute: 100000", "inconnu: 1", "duree_vie: -1h"} {
		if _, err := lire(t, minimal+"\nmetadonnees: { "+mauvais+" }\n"); err == nil {
			t.Errorf("%q accepte", mauvais)
		}
	}
}

// US-30 / US-31 : defauts, surcharge et refus des reglages de la conversion du direct.
func TestConversionDuDirectDefautsEtValidation(t *testing.T) {
	c, err := lire(t, minimal)
	l := c.Live
	if err != nil || l.DureeSonde.Std() != 10*time.Minute || l.HauteurMaxReencodage != 1080 || !l.ReencodageVideo ||
		l.DossierHLS != "hls_live" || l.InactiviteTranscodage.Std() != time.Minute || l.SegmentHLS != 4 || l.ListeHLS != 6 ||
		l.DelaiDemarrageTranscodage <= 0 || l.DelaiSonde <= 0 || l.EchecSonde <= 0 || l.SegmentsAmorce != 2 {
		t.Fatalf("defauts : %+v %v", l, err)
	}
	c, err = lire(t, minimal+"\nlive: { reencodage_video: false, hauteur_max_reencodage: 720, dossier_hls: /var/tmp/h, duree_sonde: 1m }\n")
	if err != nil || c.Live.ReencodageVideo || c.Live.HauteurMaxReencodage != 720 || c.Live.DossierHLS != "/var/tmp/h" || c.Live.DureeSonde.Std() != time.Minute {
		t.Fatalf("surcharge : %+v %v", c.Live, err)
	}
	for _, mauvais := range []string{"duree_sonde: 0s", "echec_sonde: 0s", "delai_sonde: 0s", "delai_sonde: 5m", "hauteur_max_reencodage: 0",
		"hauteur_max_reencodage: 99999", `dossier_hls: ""`, `dossier_hls: "a%d"`, "inactivite_transcodage: 0s", "delai_demarrage_transcodage: 0s",
		"delai_demarrage_transcodage: 1h", "segments_amorce: 0", "segments_amorce: 99", "segment_hls: 0", "segment_hls: 99",
		"liste_hls: 1", "liste_hls: 500", "inconnu: 1"} {
		if _, err := lire(t, minimal+"\nlive: { "+mauvais+" }\n"); err == nil {
			t.Errorf("%q accepte", mauvais)
		}
	}
}

// US-34 CA1 : defauts du retrait progressif et refus des reglages incoherents.
func TestBailRetraitProgressifDefautsEtValidation(t *testing.T) {
	c, err := lire(t, minimal)
	if err != nil {
		t.Fatal(err)
	}
	b := c.Bail
	if b.Reessai.Std() != 3*time.Second || b.ReessaiMax.Std() != 30*time.Second || b.Gigue != 0.2 ||
		b.RetryApres.Std() != 15*time.Second || b.RetryApresMax.Std() != 60*time.Second ||
		c.Supervision.CacheOccupation.Std() != 15*time.Second {
		t.Errorf("defauts : %+v", b)
	}
	cas := []struct{ yaml, attendu string }{
		{"bail: { reessai: 10s, reessai_max: 5s }", "reessai_max doit etre >= bail.reessai"},
		{"bail: { gigue: 0.9 }", "bail.gigue"},
		{"bail: { gigue: -0.1 }", "bail.gigue"},
		{"bail: { retry_apres: 30s, retry_apres_max: 10s }", "retry_apres_max"},
		{"bail: { succes_prise: 0 }", "succes_prise"},
		{"bail: { fenetre_prise: 0s }", "fenetre_prise"},
		{"supervision: { cache_occupation: 0s }", "cache_occupation"},
	}
	for _, k := range cas {
		_, err := lire(t, minimal+"\n"+k.yaml+"\n")
		if err == nil || !strings.Contains(err.Error(), k.attendu) {
			t.Errorf("%s : attendu %q, obtenu %v", k.yaml, k.attendu, err)
		}
	}
	if _, err := lire(t, minimal+"\nbail: { gigue: 0 }\n"); err != nil {
		t.Errorf("gigue 0 doit etre valide : %v", err)
	}
}
