package hls

import (
	"encoding/base64"
	"net/url"
	"strings"
	"testing"
	"time"
)

var secretTest = []byte("0123456789abcdef0123456789abcdef")

const prefixeTest = "/api/v1/live/12/seg"

func nouveauTest(t *testing.T, opt Options) *Reecrivain {
	t.Helper()
	if opt.Capacite == 0 {
		opt.Capacite = 600
	}
	if opt.Validite == 0 {
		opt.Validite = 10 * time.Minute
	}
	r, err := Nouveau(secretTest, opt)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func baseTest() *url.URL {
	b, _ := url.Parse("http://fournisseur.test/live/x/y/12.m3u8")
	return b
}

// lignesDeJetons rend les lignes de segment (celles qui ne sont pas des balises).
func lignesDeJetons(sortie string) []string {
	var r []string
	for _, l := range strings.Split(sortie, "\n") {
		if l != "" && !strings.HasPrefix(l, "#") {
			r = append(r, l)
		}
	}
	return r
}

func jetonDe(ligne string) string { return strings.TrimPrefix(ligne, prefixeTest+"/") }

const playlistFournisseur = "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:10\n#EXT-X-MEDIA-SEQUENCE:5\n" +
	"#EXTINF:10.0,\nseg5.ts\n#EXTINF:10.0,\n/autre/seg6.ts\n#EXTINF:10.0,\nhttp://cdn.exemple/seg7.ts\n"

// CA1 : chaque URL est reecrite vers le service, et le jeton redonne bien l'URL du fournisseur.
func TestReecritureResoutEtSigne(t *testing.T) {
	r := nouveauTest(t, Options{})
	sortie := r.Reecrire(prefixeTest, "12", baseTest(), playlistFournisseur)
	lignes := lignesDeJetons(sortie)
	if len(lignes) != 3 {
		t.Fatalf("3 segments attendus, %d :\n%s", len(lignes), sortie)
	}
	attendus := []string{"http://fournisseur.test/live/x/y/seg5.ts", "http://fournisseur.test/autre/seg6.ts", "http://cdn.exemple/seg7.ts"}
	for i, l := range lignes {
		if !strings.HasPrefix(l, prefixeTest+"/") {
			t.Errorf("ligne %d non reecrite : %q", i, l)
		}
		c, err := r.Resoudre("12", jetonDe(l))
		if err != nil || c.URL != attendus[i] {
			t.Errorf("segment %d : %q (%v), attendu %q", i, c.URL, err, attendus[i])
		}
	}
	if !strings.Contains(sortie, "#EXT-X-MEDIA-SEQUENCE:5") {
		t.Error("les balises doivent etre conservees")
	}
}

func nouveauAvec(t *testing.T, secret []byte) *Reecrivain {
	t.Helper()
	r, err := Nouveau(secret, Options{Capacite: 10, Validite: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// CA1 : un jeton falsifie est refuse : autre secret, N'IMPORTE QUEL octet modifie
// (nonce, charge ou etiquette), jeton tronque, URL brute.
func TestJetonFalsifieRefuse(t *testing.T) {
	r := nouveauTest(t, Options{})
	autre := nouveauAvec(t, []byte("un-autre-secret-de-32-caracteres!"))
	j := r.signer("12", "http://pirate.exemple/x")
	if _, err := r.Resoudre("12", j); err != nil {
		t.Fatalf("le jeton authentique doit etre accepte : %v", err)
	}
	if _, err := autre.Resoudre("12", j); err == nil {
		t.Error("un autre secret ne doit pas valider le jeton")
	}
	brut, _ := base64.RawURLEncoding.DecodeString(j)
	for i := range brut {
		mod := append([]byte(nil), brut...)
		mod[i] ^= 0x01
		if _, err := r.Resoudre("12", base64.RawURLEncoding.EncodeToString(mod)); err == nil {
			t.Errorf("octet %d modifie : le jeton doit etre refuse", i)
		}
	}
	if _, err := r.Resoudre("12", j[:len(j)-2]); err == nil {
		t.Error("un jeton tronque doit etre refuse")
	}
	for _, mauvais := range []string{"", "abc", "a.b.c", "..", "http://pirate.exemple/x", base64.RawURLEncoding.EncodeToString([]byte("http://pirate.exemple/x"))} {
		if _, err := r.Resoudre("12", mauvais); err == nil {
			t.Errorf("%q ne doit pas etre accepte", mauvais)
		}
	}
}

// CA1/CA5 : le client ne peut RIEN lire dans le jeton (ni hote, ni identifiants, ni
// chemin, ni nom de segment) : la charge est chiffree, pas seulement signee.
func TestJetonNeRevelePasLURL(t *testing.T) {
	r := nouveauTest(t, Options{})
	base, _ := url.Parse("http://hote-secret.test/live/UTILISATEUR/MOTDEPASSE/12.m3u8")
	pl := "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"cle-privee.bin\"\n#EXT-X-STREAM-INF:BANDWIDTH=1\nhaute/index.m3u8\n#EXTINF:1,\nsegment-secret.ts\n"
	sortie := r.Reecrire(prefixeTest, "12", base, pl)
	jetons := []string{r.signer("12", base.String())}
	for _, l := range strings.Split(sortie, "\n") {
		if i := strings.Index(l, prefixeTest+"/"); i >= 0 {
			jetons = append(jetons, strings.SplitN(l[i+len(prefixeTest)+1:], `"`, 2)[0])
		}
	}
	if len(jetons) != 4 {
		t.Fatalf("4 jetons attendus (cle, sous-playlist, segment + celui du test) : %d\n%s", len(jetons), sortie)
	}
	interdits := []string{"hote-secret", "UTILISATEUR", "MOTDEPASSE", "live", "cle-privee", "haute", "index", "segment-secret", "http", "seg:"}
	for _, j := range jetons {
		brut, err := base64.RawURLEncoding.DecodeString(strings.Split(j, ".")[0])
		if err != nil {
			t.Fatalf("jeton non decodable en base64 : %q", j)
		}
		for _, f := range interdits {
			if strings.Contains(string(brut), f) || strings.Contains(j, f) {
				t.Errorf("le fragment %q est lisible dans le jeton %q", f, j)
			}
		}
	}
	for _, f := range append(interdits[:3:3], interdits[4:7]...) { // « live » est dans le prefixe de nos routes
		if strings.Contains(sortie, f) {
			t.Errorf("%q apparait dans la playlist reecrite", f)
		}
	}
}

// Un meme contenu donne toujours le meme jeton : c'est ce qui garde les URL stables.
func TestJetonDeterministe(t *testing.T) {
	r := nouveauTest(t, Options{})
	if r.signer("12", "http://a.test/x") != r.signer("12", "http://a.test/x") {
		t.Error("meme charge, meme jeton")
	}
	if r.signer("12", "http://a.test/x") == r.signer("12", "http://a.test/y") {
		t.Error("charges differentes, jetons differents")
	}
}

func TestSecretTropCourtEtTableNonConfigureeRefuses(t *testing.T) {
	if _, err := Nouveau([]byte("court"), Options{Capacite: 1, Validite: time.Minute}); err == nil {
		t.Error("un secret court doit etre refuse")
	}
	if _, err := Nouveau(secretTest, Options{}); err == nil {
		t.Error("une table sans capacite ni validite doit etre refusee : pas de reglage cache")
	}
}

// CA1 : l'attribut URI="..." (cle, segment d'initialisation) est reecrit lui aussi.
func TestReecritureAttributURI(t *testing.T) {
	r := nouveauTest(t, Options{})
	base, _ := url.Parse("http://f.test/a/b.m3u8")
	sortie := r.Reecrire(prefixeTest, "1", base,
		"#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"cle.bin\"\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:5,\ns.ts\n")
	if strings.Contains(sortie, "cle.bin") || strings.Contains(sortie, "init.mp4") {
		t.Fatalf("les URI doivent etre reecrites : %s", sortie)
	}
	attendus := map[string]string{"KEY": "http://f.test/a/cle.bin", "MAP": "http://f.test/a/init.mp4"}
	for balise, attendu := range attendus {
		c, err := r.Resoudre("1", jetonDeAttribut(t, sortie, "#EXT-X-"+balise))
		if err != nil || c.URL != attendu {
			t.Errorf("%s : %q (%v), attendu %q", balise, c.URL, err, attendu)
		}
	}
}

// jetonDeAttribut extrait le jeton de l'attribut URI de la ligne qui commence par `balise`.
func jetonDeAttribut(t *testing.T, sortie, balise string) string {
	t.Helper()
	for _, l := range strings.Split(sortie, "\n") {
		if strings.HasPrefix(l, balise) {
			i := strings.Index(l, `URI="`+prefixeTest+"/")
			if i < 0 {
				t.Fatalf("pas d'URI reecrite dans %q", l)
			}
			return strings.SplitN(l[i+len(`URI="`+prefixeTest+"/"):], `"`, 2)[0]
		}
	}
	t.Fatalf("balise %s absente :\n%s", balise, sortie)
	return ""
}

// CA4 : les attributs URI qui designent une sous-playlist (EXT-X-MEDIA audio,
// sous-titres) sont reconnus comme telles, pour que le relais les reecrive.
func TestPlaylistMaitresseAttributsMediaReecritsEnSousPlaylists(t *testing.T) {
	r := nouveauTest(t, Options{})
	base, _ := url.Parse("http://f.test/live/12.m3u8")
	maitre := "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",NAME=\"fr\",URI=\"audio/fr.m3u8?x=1\"\n" +
		"#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID=\"s\",NAME=\"fr\",URI=\"http://cdn.test/st.m3u8\"\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=1,AUDIO=\"a\"\nvideo.m3u8\n"
	sortie := r.Reecrire(prefixeTest, "12", base, maitre)
	for balise, attendu := range map[string]string{
		"#EXT-X-MEDIA:TYPE=AUDIO":     "http://f.test/live/audio/fr.m3u8?x=1",
		"#EXT-X-MEDIA:TYPE=SUBTITLES": "http://cdn.test/st.m3u8",
	} {
		jeton := jetonDeAttribut(t, sortie, balise)
		if !strings.HasSuffix(jeton, ".m3u8") {
			t.Errorf("%s : l'URI d'une sous-playlist doit garder .m3u8 : %q", balise, jeton)
		}
		if c, err := r.Resoudre("12", jeton); err != nil || c.URL != attendu {
			t.Errorf("%s : %q (%v), attendu %q", balise, c.URL, err, attendu)
		}
	}
	if r.table.taille() != 0 {
		t.Error("une sous-playlist n'entre pas dans la table des segments")
	}
}

// CA2 : les URL reecrites sont relatives a la racine, sans hote ni schema.
func TestUrlsRelativesALaRacine(t *testing.T) {
	r := nouveauTest(t, Options{})
	sortie := r.Reecrire(prefixeTest, "12", baseTest(), playlistFournisseur)
	for _, l := range lignesDeJetons(sortie) {
		if !strings.HasPrefix(l, "/api/v1/") || strings.Contains(l, "//") || strings.Contains(l, "http") {
			t.Errorf("URL non relative a la racine : %q", l)
		}
	}
}

func playlistAvecHash(hash string) string {
	return "#EXTM3U\n#EXT-X-TARGETDURATION:10\n#EXT-X-MEDIA-SEQUENCE:153\n#EXTINF:10,\n/hls/" + hash + "/845410_153.ts\n#EXTINF:10,\n/hls/" + hash + "/845410_154.ts\n"
}

// CA3 : regression « media sequence mismatch ». Le fournisseur change le hash de
// chemin entre deux playlists ; la ligne reecrite d'un meme segment reste identique,
// et le jeton mene toujours a l'adresse la plus RECENTE.
func TestUrlsDeSegmentsStablesMalgreLeHashDuFournisseur(t *testing.T) {
	r := nouveauTest(t, Options{})
	base, _ := url.Parse("http://f.test/live/u/p/845410.m3u8")
	a := r.Reecrire(prefixeTest, "845410", base, playlistAvecHash("aaaa"))
	b := r.Reecrire(prefixeTest, "845410", base, playlistAvecHash("bbbb"))
	if a != b {
		t.Fatalf("les playlists reecrites doivent etre identiques :\n%s\n%s", a, b)
	}
	c, err := r.Resoudre("845410", jetonDe(lignesDeJetons(b)[0]))
	if err != nil || c.URL != "http://f.test/hls/bbbb/845410_153.ts" {
		t.Errorf("l'adresse la plus recente doit etre retenue : %q (%v)", c.URL, err)
	}
	if c.Cle != "seg:845410/845410_153.ts" {
		t.Errorf("la cle de cache doit etre stable : %q", c.Cle)
	}
}

// CA3 : deux chaines (portees) avec des noms de segments identiques ne se melangent pas.
func TestPorteesIsolees(t *testing.T) {
	r := nouveauTest(t, Options{})
	b1, _ := url.Parse("http://f.test/un/a.m3u8")
	b2, _ := url.Parse("http://f.test/deux/a.m3u8")
	l1 := lignesDeJetons(r.Reecrire(prefixeTest, "1", b1, "#EXTM3U\nseg.ts\n"))[0]
	l2 := lignesDeJetons(r.Reecrire(prefixeTest, "2", b2, "#EXTM3U\nseg.ts\n"))[0]
	c1, _ := r.Resoudre("1", jetonDe(l1))
	c2, _ := r.Resoudre("2", jetonDe(l2))
	if c1.URL != "http://f.test/un/seg.ts" || c2.URL != "http://f.test/deux/seg.ts" {
		t.Errorf("portees melangees : %q %q", c1.URL, c2.URL)
	}
}

// Dette lot 1 : un jeton (segment, sous-playlist, cle) n'est valable que pour la chaine
// de son chemin ; un jeton d'une autre chaine est refuse.
func TestJetonLieALaChaine(t *testing.T) {
	r := nouveauTest(t, Options{})
	b, _ := url.Parse("http://f.test/un/a.m3u8")
	sortie := r.Reecrire(prefixeTest, "1", b, "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"http://f.test/cle.bin\"\nseg.ts\nsous/autre.m3u8\n")
	n := 0
	for _, l := range lignesDeJetons(sortie) {
		j := jetonDe(l)
		if _, err := r.Resoudre("1", j); err != nil {
			t.Errorf("la bonne chaine doit marcher : %v", err)
		}
		if _, err := r.Resoudre("2", j); err != ErrJeton {
			t.Errorf("jeton d'une autre chaine accepte : %v", err)
		}
		n++
	}
	if j := jetonDeAttribut(t, sortie, "#EXT-X-KEY"); true {
		if _, err := r.Resoudre("2", j); err != ErrJeton {
			t.Errorf("jeton de cle d'une autre chaine accepte : %v", err)
		}
	}
	if n == 0 {
		t.Fatal("aucun jeton teste")
	}
}

// CA3 : la table est bornee en nombre et purgee par l'age.
func TestTableBorneeEtPurgee(t *testing.T) {
	maintenant := time.Unix(1_000_000, 0)
	r := nouveauTest(t, Options{Capacite: 3, Validite: time.Minute, Horloge: func() time.Time { return maintenant }})
	base, _ := url.Parse("http://f.test/a.m3u8")
	ecrire := func(nom string) string {
		return jetonDe(lignesDeJetons(r.Reecrire(prefixeTest, "c", base, "#EXTM3U\n"+nom+"\n"))[0])
	}
	var jetons []string
	for _, n := range []string{"s1.ts", "s2.ts", "s3.ts", "s4.ts", "s5.ts"} {
		jetons = append(jetons, ecrire(n))
		maintenant = maintenant.Add(time.Second)
	}
	if n := r.table.taille(); n != 3 {
		t.Fatalf("la table doit rester a 3 entrees, %d", n)
	}
	if _, err := r.Resoudre("c", jetons[0]); err != ErrInconnu {
		t.Errorf("le plus ancien doit etre oublie : %v", err)
	}
	if _, err := r.Resoudre("c", jetons[4]); err != nil {
		t.Errorf("le plus recent doit rester : %v", err)
	}
	maintenant = maintenant.Add(2 * time.Minute)
	if _, err := r.Resoudre("c", jetons[4]); err != ErrInconnu {
		t.Errorf("une entree perimee doit etre refusee : %v", err)
	}
	ecrire("s6.ts")
	ecrire("s7.ts")
	ecrire("s8.ts")
	ecrire("s9.ts") // declenche la purge des perimees
	if n := r.table.taille(); n > 3 {
		t.Errorf("table non purgee : %d", n)
	}
}

// CA4 : une playlist maitresse voit ses sous-playlists reecrites en jetons signes
// qui, une fois resolus, donnent l'adresse de la sous-playlist.
func TestPlaylistMaitresseSousPlaylistsReecrites(t *testing.T) {
	r := nouveauTest(t, Options{})
	base, _ := url.Parse("http://f.test/live/x/y/12.m3u8")
	maitre := "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=800000\nhaute/index.m3u8?t=1\n#EXT-X-STREAM-INF:BANDWIDTH=300000\nhttp://cdn.test/basse.m3u8\n"
	sortie := r.Reecrire(prefixeTest, "12", base, maitre)
	lignes := lignesDeJetons(sortie)
	if len(lignes) != 2 {
		t.Fatalf("2 sous-playlists attendues :\n%s", sortie)
	}
	attendus := []string{"http://f.test/live/x/y/haute/index.m3u8?t=1", "http://cdn.test/basse.m3u8"}
	for i, l := range lignes {
		if !strings.HasSuffix(l, ".m3u8") {
			t.Errorf("une sous-playlist doit garder l'extension .m3u8 : %q", l)
		}
		c, err := r.Resoudre("12", jetonDe(l))
		if err != nil || c.URL != attendus[i] {
			t.Errorf("sous-playlist %d : %q (%v)", i, c.URL, err)
		}
	}
	if !strings.Contains(sortie, "BANDWIDTH=800000") {
		t.Error("les balises STREAM-INF doivent etre conservees")
	}
}

// CA5 : aucune URL externe n'apparait en clair, et ni une URL brute, ni une adresse
// encodee sans signature, ni un nom de segment jamais emis ne donne acces a quoi que ce soit.
func TestAucuneUrlExterneLisibleSansSignature(t *testing.T) {
	r := nouveauTest(t, Options{})
	pl := playlistFournisseur + "#EXT-X-KEY:METHOD=AES-128,URI=\"http://cle.exemple/k\"\n#EXTINF:1,\nfile:///etc/passwd\n#EXTINF:1,\nseg:triche\n"
	sortie := r.Reecrire(prefixeTest, "12", baseTest(), pl)
	for _, interdit := range []string{"fournisseur.test", "cdn.exemple", "cle.exemple", "etc/passwd", "triche"} {
		if strings.Contains(sortie, interdit) {
			t.Errorf("%q ne doit pas apparaitre dans la sortie :\n%s", interdit, sortie)
		}
	}
	if n := len(lignesDeJetons(sortie)); n != 3 {
		t.Errorf("seuls les 3 segments http(s) donnent des lignes, %d :\n%s", n, sortie)
	}
	// Charge sans signature, ou signature calculee sur une autre charge.
	for _, j := range []string{"aHR0cDovL2Euc3Q", "aHR0cDovL2Euc3Q.AAAA", "c2VnOnR1ZQ.AAAA"} {
		if _, err := r.Resoudre("12", j); err == nil {
			t.Errorf("%q ne doit pas etre resolu", j)
		}
	}
	// Un nom de segment jamais emis par nous : signe correctement mais absent de la table.
	if _, err := r.Resoudre("12", r.signer("12", prefixeCle+"12/jamais-vu.ts")); err != ErrInconnu {
		t.Errorf("segment inconnu : %v", err)
	}
	// Meme signe, un schema non http(s) est refuse a la resolution.
	if _, err := r.Resoudre("12", r.signer("12", "file:///etc/passwd")); err == nil {
		t.Error("un schema non http(s) doit etre refuse")
	}
}

func TestExtensionDeSegmentConservee(t *testing.T) {
	r := nouveauTest(t, Options{})
	base, _ := url.Parse("http://f.test/a.m3u8")
	l := lignesDeJetons(r.Reecrire(prefixeTest, "c", base, "#EXTM3U\ns1.m4s\nsansextension\n"))
	if !strings.HasSuffix(l[0], ".m4s") || !strings.HasSuffix(l[1], ".ts") {
		t.Errorf("extensions : %v", l)
	}
	for _, x := range l {
		if _, err := r.Resoudre("c", jetonDe(x)); err != nil {
			t.Errorf("%q : %v", x, err)
		}
	}
}
