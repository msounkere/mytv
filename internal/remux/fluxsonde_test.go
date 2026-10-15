package remux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mytv/internal/domain"
)

// srcFilm : une source en memoire qui connait sa taille totale (comme celle du service
// de films), compte les ouvertures et la concurrence.
type srcFilm struct {
	contenu    []byte
	sansTaille bool
	ouvertures atomic.Int32
	enCours    atomic.Int32
	maxEnCours atomic.Int32
	departs    []int64
	mu         sync.Mutex
	coupeA     int // si > 0 : le corps s'interrompt apres autant d'octets
}

type lecteurFilm struct {
	io.Reader
	f     *srcFilm
	total int64
	une   sync.Once
}

func (l *lecteurFilm) Close() error  { l.une.Do(func() { l.f.enCours.Add(-1) }); return nil }
func (l *lecteurFilm) Taille() int64 { return l.total }

type lecteurFilmSansTaille struct{ io.ReadCloser }

func (f *srcFilm) source() domain.SourceOctets {
	return func(_ context.Context, depuis int64) (io.ReadCloser, error) {
		f.ouvertures.Add(1)
		n := f.enCours.Add(1)
		for {
			m := f.maxEnCours.Load()
			if n <= m || f.maxEnCours.CompareAndSwap(m, n) {
				break
			}
		}
		f.mu.Lock()
		f.departs = append(f.departs, depuis)
		f.mu.Unlock()
		if depuis > int64(len(f.contenu)) {
			f.enCours.Add(-1)
			return nil, domain.ErrPlageHorsFichier{Total: int64(len(f.contenu))}
		}
		var r io.Reader = bytes.NewReader(f.contenu[depuis:])
		if f.coupeA > 0 && len(f.contenu[depuis:]) > f.coupeA {
			r = io.MultiReader(bytes.NewReader(f.contenu[depuis:depuis+int64(f.coupeA)]), &erreurLecture{io.ErrUnexpectedEOF})
		}
		l := &lecteurFilm{Reader: r, f: f, total: int64(len(f.contenu))}
		if f.sansTaille {
			return lecteurFilmSansTaille{io.NopCloser(l)}, nil
		}
		return l, nil
	}
}

// ffprobeHTTP : un faux ffprobe qui lit l'adresse recue en argument comme le vrai : HEAD,
// lecture du debut, saut a la fin (suffixe), reprise a un decalage.
type ffprobeHTTP struct {
	mu    sync.Mutex
	args  []string
	faire func(url string) error
}

func (e *ffprobeHTTP) Executer(ctx context.Context, nom string, args []string, max int) ([]byte, error) {
	e.mu.Lock()
	e.args = append([]string{nom}, args...)
	e.mu.Unlock()
	url := args[len(args)-1]
	if e.faire != nil {
		if err := e.faire(url); err != nil {
			return nil, err
		}
	}
	return []byte(sortieMkv), nil
}

func (e *ffprobeHTTP) Demarrer(context.Context, string, []string, io.ReadCloser) (domain.Processus, error) {
	return nil, errors.New("non utilise")
}

func get(t *testing.T, url, plage, methode string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(methode, url, nil)
	if plage != "" {
		req.Header.Set("Range", plage)
	}
	rep, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer rep.Body.Close()
	b, _ := io.ReadAll(rep.Body)
	return rep, b
}

// US-17 CA1 (sonde) : ffprobe ne recoit ni l'adresse ni les identifiants du fournisseur,
// seulement une adresse locale a usage unique, et l'ecoute disparait avec l'analyse.
func TestCA1_SondeParAdresseLocale(t *testing.T) {
	film := &srcFilm{contenu: film(50_000)}
	exe := &ffprobeHTTP{}
	var vue string
	exe.faire = func(u string) error { vue = u; return nil }
	s := NouveauSondeur(ReglagesSonde{Ffprobe: "ffprobe", Duree: time.Hour, Delai: 5 * time.Second, Max: 3}, exe, nil)
	if _, err := s.Sonder(context.Background(), "iptva:1", film.source()); err != nil {
		t.Fatal(err)
	}
	ligne := strings.Join(exe.args, " ")
	if !regexp.MustCompile(`http://127\.0\.0\.1:\d+/[0-9a-f]{32}/film$`).MatchString(vue) {
		t.Errorf("adresse donnee a ffprobe : %q", vue)
	}
	for _, interdit := range []string{"fournisseur", "iptva", "utilisateur", "movie", "https", "@"} {
		if strings.Contains(ligne, interdit) {
			t.Errorf("la ligne de commande contient %q : %s", interdit, ligne)
		}
	}
	if !contient(exe.args, "-protocol_whitelist", "http,tcp") {
		t.Errorf("protocoles non limites : %v", exe.args)
	}
	// L'ecoute a disparu : l'adresse n'est plus joignable.
	hote := strings.TrimPrefix(vue, "http://")
	hote, _, _ = strings.Cut(hote, "/")
	if c, err := net.DialTimeout("tcp", hote, 500*time.Millisecond); err == nil {
		c.Close()
		t.Error("l'ecoute de la sonde est restee ouverte")
	}
}

// Ce que ffprobe attend d'un serveur HTTP : HEAD, plage fermee, plage ouverte, suffixe,
// hors fichier, sans plage ; octets exacts ; une seule connexion amont a la fois.
func TestFluxSondeRepondCommeUnServeurDeFichier(t *testing.T) {
	film := &srcFilm{contenu: film(100_000)}
	exe := &ffprobeHTTP{}
	exe.faire = func(u string) error {
		rep, corps := get(t, u, "", "HEAD")
		if rep.StatusCode != 200 || rep.ContentLength != 100_000 || rep.Header.Get("Accept-Ranges") != "bytes" || len(corps) != 0 {
			t.Errorf("HEAD : %d, longueur %d, %v", rep.StatusCode, rep.ContentLength, rep.Header)
		}
		cas := []struct {
			plage        string
			debut, fin   int
			contentRange string
		}{
			{"bytes=0-99", 0, 99, "bytes 0-99/100000"},
			{"bytes=1000-1999", 1000, 1999, "bytes 1000-1999/100000"},
			{"bytes=99990-", 99990, 99999, "bytes 99990-99999/100000"},
			{"bytes=-10", 99990, 99999, "bytes 99990-99999/100000"},
			{"bytes=99000-200000", 99000, 99999, "bytes 99000-99999/100000"}, // fin tronquee a la taille
			{"bytes=-500000", 0, 99999, "bytes 0-99999/100000"},              // suffixe plus grand que le fichier
			{"bytes=5-", 5, 99999, "bytes 5-99999/100000"},
		}
		for _, c := range cas {
			rep, corps := get(t, u, c.plage, "GET")
			if rep.StatusCode != 206 || rep.Header.Get("Content-Range") != c.contentRange || !bytes.Equal(corps, film.contenu[c.debut:c.fin+1]) {
				t.Errorf("%s : %d, %q, %d octets", c.plage, rep.StatusCode, rep.Header.Get("Content-Range"), len(corps))
			}
		}
		rep, corps = get(t, u, "", "GET")
		if rep.StatusCode != 200 || !bytes.Equal(corps, film.contenu) {
			t.Errorf("sans plage : %d, %d octets", rep.StatusCode, len(corps))
		}
		for _, hors := range []string{"bytes=100000-", "bytes=999999-1000000"} {
			rep, _ = get(t, u, hors, "GET")
			if rep.StatusCode != 416 || rep.Header.Get("Content-Range") != "bytes */100000" {
				t.Errorf("%s : %d %q", hors, rep.StatusCode, rep.Header.Get("Content-Range"))
			}
		}
		// Plage illisible : ignoree (200 complet), comme le veut la RFC.
		if rep, corps = get(t, u, "bytes=a-b", "GET"); rep.StatusCode != 200 || len(corps) != 100_000 {
			t.Errorf("plage illisible : %d, %d octets", rep.StatusCode, len(corps))
		}
		// Autre chemin ou autre methode : refuses.
		base := u[:strings.LastIndex(u, "/")]
		if rep, _ = get(t, base+"/autre", "", "GET"); rep.StatusCode != 404 {
			t.Errorf("autre chemin : %d", rep.StatusCode)
		}
		if rep, _ = get(t, u, "", "POST"); rep.StatusCode != 405 {
			t.Errorf("POST : %d", rep.StatusCode)
		}
		if rep, _ = get(t, strings.Replace(u, "/film", "/", 1), "", "GET"); rep.StatusCode != 404 {
			t.Errorf("sans jeton : %d", rep.StatusCode)
		}
		return nil
	}
	s := NouveauSondeur(ReglagesSonde{Ffprobe: "ffprobe", Duree: time.Hour, Delai: 10 * time.Second, Max: 3}, exe, nil)
	if _, err := s.Sonder(context.Background(), "iptva:1", film.source()); err != nil {
		t.Fatal(err)
	}
	if film.maxEnCours.Load() != 1 {
		t.Errorf("%d connexions amont simultanees au plus attendues 1", film.maxEnCours.Load())
	}
	if film.enCours.Load() != 0 {
		t.Errorf("%d lectures amont restees ouvertes", film.enCours.Load())
	}
}

// Des requetes simultanees (ffprobe peut en ouvrir plusieurs) ne font jamais deux
// connexions amont a la fois : le fournisseur n'en accorde qu'une.
func TestFluxSondeSerialiseLAmont(t *testing.T) {
	film := &srcFilm{contenu: film(2_000_000)}
	exe := &ffprobeHTTP{}
	exe.faire = func(u string) error {
		var wg sync.WaitGroup
		for i := 0; i < 6; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				plage := fmt.Sprintf("bytes=%d-%d", i*1000, i*1000+99999)
				_, corps := get(t, u, plage, "GET")
				if len(corps) != 100_000 {
					t.Errorf("requete %d : %d octets", i, len(corps))
				}
			}(i)
		}
		wg.Wait()
		return nil
	}
	s := NouveauSondeur(ReglagesSonde{Ffprobe: "ffprobe", Duree: time.Hour, Delai: 10 * time.Second, Max: 3}, exe, nil)
	if _, err := s.Sonder(context.Background(), "iptva:2", film.source()); err != nil {
		t.Fatal(err)
	}
	if m := film.maxEnCours.Load(); m != 1 {
		t.Errorf("%d connexions amont simultanees", m)
	}
}

// L'erreur du fournisseur (404, 407...) est rendue TELLE QUELLE, typee, au lieu d'un
// « ffprobe a echoue » : l'API peut repondre 404 ou 503 au lieu de 502.
func TestSondeRendLErreurTypeeDuFournisseur(t *testing.T) {
	for _, cause := range []error{domain.ErrIntrouvable, domain.ErrRefuseConnexions, domain.ErrIdentifiants, domain.ErrAmont} {
		exe := &ffprobeHTTP{}
		exe.faire = func(u string) error {
			rep, _ := get(t, u, "", "GET")
			if rep.StatusCode != 502 {
				t.Errorf("%v : ffprobe voit %d au lieu de 502", cause, rep.StatusCode)
			}
			return errors.New("ffprobe : exit status 1") // ffprobe echoue, comme avec un 502
		}
		s := NouveauSondeur(ReglagesSonde{Ffprobe: "ffprobe", Duree: time.Hour, Delai: 5 * time.Second, Max: 3}, exe, nil)
		src := func(context.Context, int64) (io.ReadCloser, error) { return nil, fmt.Errorf("source: %w", cause) }
		_, err := s.Sonder(context.Background(), "iptva:3", src)
		if !errors.Is(err, cause) {
			t.Errorf("%v : erreur rendue %v", cause, err)
		}
		if len(s.cache) != 0 {
			t.Error("un echec est entre dans le cache")
		}
	}
}

// Un amont qui s'interrompt coupe la reponse locale (ffprobe voit un fichier tronque, pas un
// fichier complet), et la cause est gardee.
func TestFluxSondeCoupureDeLAmont(t *testing.T) {
	film := &srcFilm{contenu: film(100_000), coupeA: 10_000}
	exe := &ffprobeHTTP{}
	exe.faire = func(u string) error {
		rep, err := http.Get(u)
		if err == nil {
			_, err = io.Copy(io.Discard, rep.Body)
			rep.Body.Close()
		}
		if err == nil {
			t.Error("ffprobe n'a pas vu la coupure")
		}
		return errors.New("ffprobe : fichier tronque")
	}
	s := NouveauSondeur(ReglagesSonde{Ffprobe: "ffprobe", Duree: time.Hour, Delai: 5 * time.Second, Max: 3}, exe, nil)
	_, err := s.Sonder(context.Background(), "iptva:4", film.source())
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("erreur = %v", err)
	}
}

// Sans taille connue, le flux est servi sans plage (ffprobe lira sequentiellement) plutot
// que de mentir sur une taille.
func TestFluxSondeSansTailleConnue(t *testing.T) {
	film := &srcFilm{contenu: film(30_000), sansTaille: true}
	exe := &ffprobeHTTP{}
	exe.faire = func(u string) error {
		rep, corps := get(t, u, "bytes=100-199", "GET")
		if rep.StatusCode != 200 || len(corps) != 30_000 || rep.Header.Get("Accept-Ranges") != "" || rep.Header.Get("Content-Range") != "" {
			t.Errorf("%d, %d octets, %v", rep.StatusCode, len(corps), rep.Header)
		}
		return nil
	}
	s := NouveauSondeur(ReglagesSonde{Ffprobe: "ffprobe", Duree: time.Hour, Delai: 5 * time.Second, Max: 3}, exe, nil)
	if _, err := s.Sonder(context.Background(), "iptva:5", film.source()); err != nil {
		t.Fatal(err)
	}
}

// Annuler l'analyse ferme l'ecoute et les lectures amont, sans goroutine restante.
func TestFluxSondeAnnulationSansFuite(t *testing.T) {
	avant := 0
	{
		s := NouveauSondeur(ReglagesSonde{Ffprobe: "ffprobe", Duree: time.Hour, Delai: 5 * time.Second, Max: 3}, &exeFaux{sortie: sortieMkv}, nil)
		s.Sonder(context.Background(), "iptva:0", srcVide) // chauffe
	}
	avant = goroutines()
	for i := 0; i < 5; i++ {
		film := &srcFilm{contenu: film(1_000_000)}
		ctx, annule := context.WithCancel(context.Background())
		exe := &ffprobeHTTP{}
		exe.faire = func(u string) error {
			req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
			rep, err := http.DefaultClient.Do(req)
			if err == nil {
				io.ReadFull(rep.Body, make([]byte, 100))
				annule() // l'analyse est abandonnee en pleine lecture
				rep.Body.Close()
			}
			return ctx.Err()
		}
		s := NouveauSondeur(ReglagesSonde{Ffprobe: "ffprobe", Duree: time.Hour, Delai: 5 * time.Second, Max: 3}, exe, nil)
		if _, err := s.Sonder(ctx, fmt.Sprintf("iptva:%d", i+10), film.source()); !errors.Is(err, context.Canceled) {
			t.Errorf("erreur = %v", err)
		}
		if film.enCours.Load() != 0 {
			t.Errorf("%d lectures amont ouvertes", film.enCours.Load())
		}
	}
	http.DefaultClient.CloseIdleConnections()
	sansGoroutine(t, avant)
}

// Regression (recette reelle, MKV de 6,7 Go) : pour lire l'index en fin de fichier,
// ffprobe ouvre une NOUVELLE connexion AVANT de fermer l'ancienne, qu'il ne lit plus.
// Le verrou tenu pendant tout l'envoi bloquait la nouvelle requete jusqu'au delai de
// l'analyse (« context deadline exceeded », film injouable).
func TestFluxSondeUneNouvelleRequetePasseQuandLAncienneNEstPlusLue(t *testing.T) {
	film := &srcFilm{contenu: film(40_000_000)}
	f, err := ouvrirFluxSonde(film.source())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Fermer()

	// Requete 1 : sans plage, le client lit un peu puis ne lit plus mais garde la connexion.
	req1, _ := http.NewRequest(http.MethodGet, f.url, nil)
	rep1, err := http.DefaultClient.Do(req1)
	if err != nil {
		t.Fatal(err)
	}
	defer rep1.Body.Close()
	if _, err := io.ReadFull(rep1.Body, make([]byte, 4096)); err != nil {
		t.Fatal(err)
	}

	// Requete 2 : plage vers la fin, sur une autre connexion, pendant que la 1 est ouverte.
	debut := time.Now()
	req2, _ := http.NewRequest(http.MethodGet, f.url, nil)
	req2.Header.Set("Range", "bytes=39990000-")
	client2 := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	rep2, err := client2.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer rep2.Body.Close()
	corps, err := io.ReadAll(rep2.Body)
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(debut); d > 3*time.Second {
		t.Fatalf("la nouvelle requete a attendu %v : elle doit passer apres la grace (%v)", d, graceBascule)
	}
	if len(corps) != 10_000 || !bytes.Equal(corps, film.contenu[39_990_000:]) {
		t.Fatalf("octets recus : %d, attendu 10000 identiques", len(corps))
	}
	if m := film.maxEnCours.Load(); m != 1 {
		t.Errorf("%d connexions amont simultanees (une seule autorisee)", m)
	}
	if err := f.Erreur(); err != nil {
		t.Errorf("la coupure volontaire de l'ancienne requete ne doit pas etre une erreur : %v", err)
	}
}
