package dev

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// petit : options pour les tests qui n'ont pas besoin du volume complet.
func petit() Options {
	return Options{Chaines: 30, Films: 40, Series: 10, EPGChaines: 10}
}

func demarrer(t *testing.T, o Options) *Harnais {
	t.Helper()
	h, err := Demarrer(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Fermer)
	return h
}

// client avec cookie, sans connexion persistante (pas de goroutine de transport restante).
func client(t *testing.T) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, Timeout: 20 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
}

func connecter(t *testing.T, h *Harnais, c *http.Client, nom string) {
	t.Helper()
	corps := fmt.Sprintf(`{"nom":%q,"mot_de_passe":%q}`, nom, h.MotDePasse)
	rep, err := c.Post(h.URL+"/api/v1/connexion", "application/json", strings.NewReader(corps))
	if err != nil {
		t.Fatal(err)
	}
	rep.Body.Close()
	if rep.StatusCode != 200 {
		t.Fatalf("connexion %s : %d", nom, rep.StatusCode)
	}
}

// compter parcourt toutes les pages d'une liste de l'API et rend le nombre d'elements.
func compter(t *testing.T, c *http.Client, base, chemin string) int {
	t.Helper()
	total, decalage := 0, 0
	for {
		rep, err := c.Get(fmt.Sprintf("%s%s?limite=200&decalage=%d", base, chemin, decalage))
		if err != nil {
			t.Fatal(err)
		}
		var page struct {
			Elements []json.RawMessage `json:"elements"`
			Suivant  *int              `json:"suivant"`
		}
		err = json.NewDecoder(rep.Body).Decode(&page)
		rep.Body.Close()
		if err != nil || rep.StatusCode != 200 {
			t.Fatalf("%s : %d %v", chemin, rep.StatusCode, err)
		}
		total += len(page.Elements)
		if page.Suivant == nil {
			return total
		}
		decalage = *page.Suivant
	}
}

// CA1 : le harnais demarre le faux fournisseur ET mytv, avec un utilisateur de test
// qui se connecte, et un catalogue de la taille demandee.
func TestCA1_DemarrageComplet(t *testing.T) {
	h := demarrer(t, petit())
	for _, u := range []string{h.URL, h.FournisseurURL, h.ControleURL} {
		if !strings.HasPrefix(u, "http://127.0.0.1:") {
			t.Fatalf("URL hors boucle locale : %s", u)
		}
	}
	c := client(t)
	connecter(t, h, c, h.Utilisateur)
	for chemin, attendu := range map[string]int{"/chaines": 30, "/films": 40, "/series": 10} {
		if n := compter(t, c, h.URL+"/api/v1", chemin); n != attendu {
			t.Errorf("%s : %d elements, attendu %d", chemin, n, attendu)
		}
	}
	// L'administrateur existe aussi (diagnostic, metriques).
	ca := client(t)
	connecter(t, h, ca, h.Admin)
	rep, err := ca.Get(h.URL + "/api/v1/metriques")
	if err != nil {
		t.Fatal(err)
	}
	rep.Body.Close()
	if rep.StatusCode != 200 {
		t.Errorf("metriques en administrateur : %d", rep.StatusCode)
	}
}

// CA1 (volume) : le catalogue par defaut est celui du cahier des charges, et le
// guide couvre 48 h. Aussi CA2 : demarrage en moins de 5 s.
func TestCA1_CA2_VolumeParDefautEtDemarrageRapide(t *testing.T) {
	if testing.Short() {
		t.Skip("volume complet")
	}
	debut := time.Now()
	h := demarrer(t, Options{})
	// Le detecteur de courses ralentit de 10 a 20 fois : le budget ne vaut que sans lui.
	if d := time.Since(debut); d > 5*time.Second && !avecRace {
		t.Errorf("demarrage en %s, budget 5 s", d)
	} else {
		t.Logf("demarrage en %s", d)
	}
	c := client(t)
	connecter(t, h, c, h.Utilisateur)
	for chemin, mini := range map[string]int{"/chaines": 2000, "/films": 5000, "/series": 500} {
		if n := compter(t, c, h.URL+"/api/v1", chemin); n < mini {
			t.Errorf("%s : %d elements, au moins %d attendus", chemin, n, mini)
		}
	}
	// EPG sur 48 h : une chaine a des programmes jusqu'a au moins +40 h.
	de := time.Now().UTC()
	rep, err := c.Get(fmt.Sprintf("%s/api/v1/epg/grille?chaines=iptva:1001&de=%s&a=%s", h.URL,
		de.Add(40*time.Hour).Format(time.RFC3339), de.Add(41*time.Hour).Format(time.RFC3339)))
	if err != nil {
		t.Fatal(err)
	}
	corps, _ := io.ReadAll(rep.Body)
	rep.Body.Close()
	if rep.StatusCode != 200 || !bytes.Contains(corps, []byte("debut")) {
		t.Errorf("guide a +40 h : %d %.200s", rep.StatusCode, corps)
	}
}

// CA2 : arret propre. Apres Fermer, plus de goroutine, plus de dossier temporaire,
// plus d'ecoute.
func TestCA2_ArretPropre(t *testing.T) {
	avant := runtime.NumGoroutine()
	h, err := Demarrer(context.Background(), petit())
	if err != nil {
		t.Fatal(err)
	}
	dossier := h.dossier
	adresses := []string{strings.TrimPrefix(h.URL, "http://"), strings.TrimPrefix(h.FournisseurURL, "http://"), strings.TrimPrefix(h.ControleURL, "http://")}
	if _, err := os.Stat(dossier); err != nil {
		t.Fatal(err)
	}
	h.Fermer()
	h.Fermer() // idempotent
	if _, err := os.Stat(dossier); !os.IsNotExist(err) {
		t.Errorf("dossier temporaire reste : %v", err)
	}
	for _, a := range adresses {
		if c, err := net.DialTimeout("tcp", a, time.Second); err == nil {
			c.Close()
			t.Errorf("%s ecoute encore", a)
		}
	}
	attendreGoroutines(t, avant)
}

func attendreGoroutines(t *testing.T, avant int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > avant+1 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > avant+1 {
		buf := make([]byte, 1<<16)
		t.Errorf("fuite de goroutines : %d avant, %d apres\n%s", avant, n, buf[:runtime.Stack(buf, true)])
	}
}

// CA2 : l'annulation du contexte arrete tout et Attendre rend la main.
func TestCA2_AnnulationDuContexte(t *testing.T) {
	ctx, annuler := context.WithCancel(context.Background())
	h, err := Demarrer(ctx, petit())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Fermer()
	fin := make(chan error, 1)
	go func() { fin <- h.Attendre() }()
	annuler()
	select {
	case err := <-fin:
		if err != nil {
			t.Errorf("arret sur erreur : %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Attendre ne rend pas la main")
	}
}

// socketsDuProcessus rend les connexions TCP (locale, distante, etat) du processus.
func socketsDuProcessus(t *testing.T) (res []string) {
	t.Helper()
	inodes := map[uint64]bool{}
	fds, _ := os.ReadDir("/proc/self/fd")
	for _, fd := range fds {
		l, err := os.Readlink(filepath.Join("/proc/self/fd", fd.Name()))
		var n uint64
		if err == nil && strings.HasPrefix(l, "socket:[") {
			fmt.Sscanf(l, "socket:[%d]", &n)
			inodes[n] = true
		}
	}
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Skip("pas de /proc/net :", err)
		}
		for _, l := range strings.Split(string(b), "\n")[1:] {
			champs := strings.Fields(l)
			if len(champs) < 10 {
				continue
			}
			var inode uint64
			fmt.Sscanf(champs[9], "%d", &inode)
			if inodes[inode] {
				res = append(res, adresseProc(champs[1])+" -> "+adresseProc(champs[2]))
			}
		}
	}
	return res
}

// adresseProc decode « 0100007F:1F90 » (IPv4) ou la forme IPv6 de /proc/net/tcp6.
func adresseProc(s string) string {
	h, p, _ := strings.Cut(s, ":")
	b, _ := hex.DecodeString(h)
	port, _ := hex.DecodeString(p)
	var ip net.IP
	switch len(b) {
	case 4:
		ip = net.IP{b[3], b[2], b[1], b[0]}
	case 16: // quatre mots de 32 bits en ordre de l'hote (petit-boutiste)
		ip = make(net.IP, 16)
		for i := 0; i < 4; i++ {
			binary.BigEndian.PutUint32(ip[i*4:], binary.LittleEndian.Uint32(b[i*4:]))
		}
	}
	return fmt.Sprintf("%s:%d", ip, binary.BigEndian.Uint16(append([]byte{0, 0}[:2-len(port)], port...)))
}

func horsBoucle(sockets []string) (mauvais []string) {
	for _, s := range sockets {
		for _, a := range strings.Split(s, " -> ") {
			h, _, _ := net.SplitHostPort(a)
			ip := net.ParseIP(h)
			// 0.0.0.0 / :: en « distante » d'une ecoute = pas de pair ; en « locale » = ecoute ouverte a tous.
			if strings.HasSuffix(a, ":0") && ip.IsUnspecified() {
				continue
			}
			if ip == nil || !ip.IsLoopback() {
				mauvais = append(mauvais, s)
				break
			}
		}
	}
	return
}

// CA2 : jamais d'hote externe. On inspecte TOUS les sockets du processus pendant
// que le harnais travaille (synchronisation, connexion, lecture du direct) : tous
// doivent etre sur la boucle locale. Le detecteur est lui-meme verifie sur une
// ecoute volontairement ouverte a toutes les interfaces.
func TestCA2_AucuneConnexionHorsBoucleLocale(t *testing.T) {
	h := demarrer(t, petit())
	c := client(t)
	connecter(t, h, c, h.Utilisateur)
	rep, err := c.Get(h.URL + "/api/v1/live/1001.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	rep.Body.Close()

	s := socketsDuProcessus(t)
	if len(s) < 3 {
		t.Fatalf("detecteur muet (%v) : il doit voir au moins les trois ecoutes", s)
	}
	if m := horsBoucle(s); len(m) > 0 {
		t.Errorf("sockets hors boucle locale : %v", m)
	}

	// Controle du detecteur.
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Skip("ecoute sur toutes les interfaces impossible :", err)
	}
	defer ln.Close()
	if m := horsBoucle(socketsDuProcessus(t)); len(m) == 0 {
		t.Error("le detecteur ne voit pas une ecoute ouverte a toutes les interfaces")
	}
}

// CA2 : les adresses non locales sont refusees, avant toute ouverture.
func TestCA2_RefuseLesAdressesNonLocales(t *testing.T) {
	for _, a := range []string{"0.0.0.0:8080", ":8080", "192.168.50.3:8080", "exemple.fr:80", "8080"} {
		for _, champ := range []string{"ecoute", "fournisseur", "controle"} {
			o := petit()
			switch champ {
			case "ecoute":
				o.Ecoute = a
			case "fournisseur":
				o.Fournisseur = a
			default:
				o.Controle = a
			}
			if _, err := Demarrer(context.Background(), o); err == nil {
				t.Errorf("--%s %q accepte", champ, a)
			}
		}
	}
}

// CA3 : l'interface est servie depuis --dist, a la meme origine que l'API.
func TestCA3_ServiInterfaceMemeOrigine(t *testing.T) {
	dist := t.TempDir()
	if err := os.WriteFile(filepath.Join(dist, "index.html"), []byte("<!doctype html><title>ui-de-test</title>"), 0o644); err != nil {
		t.Fatal(err)
	}
	o := petit()
	o.Dist = dist
	h := demarrer(t, o)
	rep, err := http.Get(h.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	corps, _ := io.ReadAll(rep.Body)
	rep.Body.Close()
	if rep.StatusCode != 200 || !bytes.Contains(corps, []byte("ui-de-test")) {
		t.Errorf("interface : %d %s", rep.StatusCode, corps)
	}
	rep, err = http.Get(h.URL + "/api/v1/sante")
	if err != nil {
		t.Fatal(err)
	}
	rep.Body.Close()
	if rep.StatusCode != 200 {
		t.Errorf("API a la meme origine : %d", rep.StatusCode)
	}
	// Dossier inexistant : refus clair.
	o.Dist = filepath.Join(dist, "absent")
	if _, err := Demarrer(context.Background(), o); err == nil {
		t.Error("--dist inexistant accepte")
	}
}

// CA4 : --panne 407:duree. Le fournisseur refuse le direct pendant la duree, puis
// redevient normal ; DELETE /panne la leve aussitot.
func TestCA4_PanneProgrammee(t *testing.T) {
	o := petit()
	o.Pannes = []string{"407:1500ms"}
	h := demarrer(t, o)
	c := client(t)
	connecter(t, h, c, h.Utilisateur)

	// Pendant la panne, le relais se heurte au 407 (il patiente : on borne l'attente).
	ctx, annuler := context.WithTimeout(context.Background(), 700*time.Millisecond)
	req, _ := http.NewRequestWithContext(ctx, "GET", h.URL+"/api/v1/live/1001.m3u8", nil)
	if rep, err := c.Do(req); err == nil {
		rep.Body.Close()
		if rep.StatusCode == 200 {
			t.Error("playlist servie pendant la panne 407")
		}
	}
	annuler()
	if n := h.Faux().Compteurs().Refus407; n == 0 {
		t.Error("le fournisseur n'a refuse aucune requete en 407")
	}
	// Apres la duree, le direct fonctionne.
	time.Sleep(1500 * time.Millisecond)
	attendu200(t, c, h.URL+"/api/v1/live/1002.m3u8")
}

func attendu200(t *testing.T, c *http.Client, url string) {
	t.Helper()
	var code int
	for i := 0; i < 20; i++ {
		rep, err := c.Get(url)
		if err == nil {
			code = rep.StatusCode
			rep.Body.Close()
			if code == 200 {
				return
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("%s : %d au lieu de 200", url, code)
}

// CA4 : pilotage a chaud des pannes par l'API de controle.
func TestCA4_PilotageDesPannes(t *testing.T) {
	h := demarrer(t, petit())
	c := client(t)
	connecter(t, h, c, h.Utilisateur)
	attendu200(t, c, h.URL+"/api/v1/live/1001.m3u8")

	rep, err := http.Post(h.ControleURL+"/panne?statut=407&duree=1h", "", nil)
	if err != nil || rep.StatusCode != 204 {
		t.Fatalf("POST /panne : %v %v", rep, err)
	}
	ctx, annuler := context.WithTimeout(context.Background(), 600*time.Millisecond)
	req, _ := http.NewRequestWithContext(ctx, "GET", h.URL+"/api/v1/live/1001.m3u8", nil)
	// La playlist peut venir du cache de fraicheur ; on demande une autre chaine.
	req.URL.Path = "/api/v1/live/1003.m3u8"
	if r, err := c.Do(req); err == nil {
		r.Body.Close()
	}
	annuler()
	if h.Faux().Compteurs().Refus407 == 0 {
		t.Error("panne non appliquee")
	}
	for _, mauvais := range []string{"statut=200&duree=1s", "statut=407", "statut=407&duree=0s", "statut=x&duree=1s"} {
		r, err := http.Post(h.ControleURL+"/panne?"+mauvais, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != 400 {
			t.Errorf("%s : %d au lieu de 400", mauvais, r.StatusCode)
		}
	}
	req2, _ := http.NewRequest("DELETE", h.ControleURL+"/panne", nil)
	r, err := http.DefaultClient.Do(req2)
	if err != nil || r.StatusCode != 204 {
		t.Fatalf("DELETE /panne : %v %v", r, err)
	}
	attendu200(t, c, h.URL+"/api/v1/live/1004.m3u8")

	r, err = http.Get(h.ControleURL + "/compteurs")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var cpt map[string]any
	if json.NewDecoder(r.Body).Decode(&cpt) != nil || cpt["Playlists"] == nil {
		t.Errorf("compteurs illisibles : %v", cpt)
	}
}

// CA4 : --latence retarde les reponses du fournisseur sur le direct.
func TestCA4_Latence(t *testing.T) {
	o := petit()
	o.Latence = 400 * time.Millisecond
	h := demarrer(t, o)
	c := client(t)
	connecter(t, h, c, h.Utilisateur)
	debut := time.Now()
	attendu200(t, c, h.URL+"/api/v1/live/1001.m3u8")
	if d := time.Since(debut); d < 400*time.Millisecond {
		t.Errorf("playlist en %s malgre une latence de 400 ms", d)
	}
}

func TestAnalyserPanne(t *testing.T) {
	ok := map[string]Panne{"407:30s": {407, 30 * time.Second}, "502:1m": {502, time.Minute}}
	for s, attendu := range ok {
		if p, err := AnalyserPanne(s); err != nil || p != attendu {
			t.Errorf("%q : %v %v", s, p, err)
		}
	}
	for _, s := range []string{"", "407", "407:", ":30s", "200:30s", "700:1s", "407:-1s", "407:abc", "x:1s"} {
		if _, err := AnalyserPanne(s); err == nil {
			t.Errorf("%q accepte", s)
		}
	}
}

func TestValiderOptions(t *testing.T) {
	var o Options
	if err := o.Valider(); err != nil {
		t.Fatalf("defauts invalides : %v", err)
	}
	if o.Chaines != 2000 || o.Films != 5000 || o.Series != 500 || o.EPGDuree != 48*time.Hour {
		t.Errorf("defauts : %+v", o)
	}
	for nom, mod := range map[string]func(*Options){
		"mot de passe court": func(o *Options) { o.MotDePasse = "court" },
		"memes comptes":      func(o *Options) { o.Admin = o.Utilisateur },
		"latence negative":   func(o *Options) { o.Latence = -1 },
		"panne invalide":     func(o *Options) { o.Pannes = []string{"abc"} },
		"epg trop grand":     func(o *Options) { o.Chaines = 10; o.EPGChaines = 11 },
	} {
		o := Options{}
		o.Valider()
		mod(&o)
		if err := o.Valider(); err == nil {
			t.Errorf("%s accepte", nom)
		}
	}
}

// Les identifiants du faux fournisseur ne sortent ni dans le journal ni dans les erreurs.
func TestIdentifiantsDuFauxFournisseurAbsentsDuJournal(t *testing.T) {
	var tampon syncBuffer
	o := petit()
	o.Journal = &tampon
	o.Verbeux = true
	h := demarrer(t, o)
	c := client(t)
	connecter(t, h, c, h.Utilisateur)
	attendu200(t, c, h.URL+"/api/v1/live/1001.m3u8")
	h.Fermer()
	if strings.Contains(tampon.String(), h.MotDePasse) {
		t.Error("mot de passe de test dans le journal")
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// US-16 (verdict US-15) : avec CompteLocal, 127.0.0.1 est dans comptes.plages_locales,
// donc un lecteur sans cookie (Cast) lit le direct, mais ne peut rien ecrire.
func TestCompteTechniqueLocalLitSansCookie(t *testing.T) {
	o := petit()
	o.CompteLocal = true
	h := demarrer(t, o)
	c := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	var code int
	for i := 0; i < 10; i++ {
		rep, err := c.Get(h.URL + "/api/v1/live/1001.m3u8")
		if err != nil {
			t.Fatal(err)
		}
		rep.Body.Close()
		if code = rep.StatusCode; code == 200 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if code != 200 {
		t.Errorf("lecture sans cookie depuis 127.0.0.1 : %d", code)
	}
	rep, err := c.Post(h.URL+"/api/v1/live/1001/fin", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	rep.Body.Close()
	if rep.StatusCode != 401 {
		t.Errorf("le compte technique ne fait que lire : /fin donne %d", rep.StatusCode)
	}
	// Il ne lit que le direct et les films : le catalogue reste reserve a la session.
	rep, err = c.Get(h.URL + "/api/v1/chaines")
	if err != nil {
		t.Fatal(err)
	}
	rep.Body.Close()
	if rep.StatusCode != 401 {
		t.Errorf("/chaines sans cookie : %d au lieu de 401", rep.StatusCode)
	}
	// Un film est lisible sans cookie (GET /vod), sans quoi Cast ne lirait rien.
	rep, err = c.Get(h.URL + "/api/v1/vod/iptva:200001")
	if err != nil {
		t.Fatal(err)
	}
	rep.Body.Close()
	if rep.StatusCode == 401 || rep.StatusCode == 403 {
		t.Errorf("/vod sans cookie refuse : %d", rep.StatusCode)
	}
}

// Par defaut le compte technique est desactive : une requete sans cookie est refusee
// partout (les scenarios de session expiree y comptent).
func TestCompteTechniqueDesactiveParDefaut(t *testing.T) {
	h := demarrer(t, petit())
	c := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	for _, chemin := range []string{"/api/v1/live/1001.m3u8", "/api/v1/vod/iptva:200001", "/api/v1/chaines"} {
		rep, err := c.Get(h.URL + chemin)
		if err != nil {
			t.Fatal(err)
		}
		rep.Body.Close()
		if rep.StatusCode != 401 {
			t.Errorf("%s sans cookie : %d au lieu de 401", chemin, rep.StatusCode)
		}
	}
}
