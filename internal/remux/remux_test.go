package remux

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mytv/internal/domain"
)

// ---------------------------------------------------------------- decision

// CA2 : le choix direct / remux / audio converti se lit sur une table.
func TestDeciderTable(t *testing.T) {
	cas := []struct {
		nom  string
		s    domain.Sonde
		mode domain.Mode
	}{
		{"mp4 h264 aac", domain.Sonde{Conteneur: "mov,mp4,m4a,3gp,3g2,mj2", CodecVideo: "h264", CodecAudio: "aac"}, domain.ModeDirect},
		{"mp4 sans audio", domain.Sonde{Conteneur: "mov,mp4,m4a,3gp,3g2,mj2", CodecVideo: "h264"}, domain.ModeDirect},
		{"mkv h264 aac", domain.Sonde{Conteneur: "matroska,webm", CodecVideo: "h264", CodecAudio: "aac"}, domain.ModeRemux},
		{"avi h264 mp3", domain.Sonde{Conteneur: "avi", CodecVideo: "h264", CodecAudio: "mp3"}, domain.ModeRemux},
		{"mkv h264 eac3", domain.Sonde{Conteneur: "matroska,webm", CodecVideo: "h264", CodecAudio: "eac3"}, domain.ModeAudioConverti},
		{"mp4 h264 ac3", domain.Sonde{Conteneur: "mov,mp4", CodecVideo: "h264", CodecAudio: "ac3"}, domain.ModeAudioConverti},
		{"mkv h264 dts majuscules", domain.Sonde{Conteneur: "Matroska,WebM", CodecVideo: "H264", CodecAudio: "DTS"}, domain.ModeAudioConverti},
		{"mkv hevc eac3", domain.Sonde{Conteneur: "matroska,webm", CodecVideo: "hevc", CodecAudio: "eac3"}, ModeRefus},
		{"mp4 hevc aac", domain.Sonde{Conteneur: "mov,mp4", CodecVideo: "hevc", CodecAudio: "aac"}, ModeRefus},
		{"rien", domain.Sonde{Conteneur: "mpegts"}, ModeRefus},
		{"audio seul aac", domain.Sonde{Conteneur: "mov,mp4", CodecAudio: "aac"}, domain.ModeDirect},
	}
	for _, c := range cas {
		t.Run(c.nom, func(t *testing.T) {
			d := Decider(&c.s)
			if d.Mode != c.mode {
				t.Errorf("mode %q, attendu %q (%s)", d.Mode, c.mode, d.Raison)
			}
			if d.Raison == "" {
				t.Error("raison vide")
			}
		})
	}
	if d := Decider(nil); d.Mode != ModeRefus {
		t.Errorf("sonde nil : %+v", d)
	}
}

// ---------------------------------------------------------------- arguments

func tache() domain.TacheRemux {
	return domain.TacheRemux{
		URLAmont: "http://fournisseur.test/movie/u/p/12.mkv",
		Entetes:  http.Header{"User-Agent": {"Mozilla/5.0 test"}},
		Mode:     domain.ModeRemux,
	}
}

func contient(a []string, suite ...string) bool {
	for i := 0; i+len(suite) <= len(a); i++ {
		ok := true
		for j := range suite {
			if a[i+j] != suite[j] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// CA3 : mp4 fragmente, flux copies ; l'audio n'est re-encode que dans son mode.
func TestArgumentsSelonLeMode(t *testing.T) {
	a, err := Arguments(tache())
	if err != nil {
		t.Fatal(err)
	}
	for _, suite := range [][]string{{"-c:v", "copy"}, {"-c:a", "copy"}, {"-f", "mp4", "pipe:1"},
		{"-i", "http://fournisseur.test/movie/u/p/12.mkv"}, {"-user_agent", "Mozilla/5.0 test"},
		{"-protocol_whitelist", protocoles}} {
		if !contient(a, suite...) {
			t.Errorf("%v absent de %v", suite, a)
		}
	}
	if contient(a, "-ss") {
		t.Error("-ss present sans position")
	}
	tc := tache()
	tc.Mode = domain.ModeAudioConverti
	tc.Debut = 90*time.Minute + 1500*time.Millisecond
	a, err = Arguments(tc)
	if err != nil {
		t.Fatal(err)
	}
	if !contient(a, "-c:a", "aac") || !contient(a, "-ss", "5401.500") {
		t.Errorf("audio converti / seek : %v", a)
	}
	// -ss doit preceder -i (recherche sur l'entree).
	var iss, ii int
	for i, x := range a {
		switch x {
		case "-ss":
			iss = i
		case "-i":
			ii = i
		}
	}
	if iss > ii {
		t.Errorf("-ss apres -i : %v", a)
	}
}

// CA5 : aucune valeur douteuse n'arrive jusqu'aux arguments.
func TestArgumentsRefusentLesEntreesDouteuses(t *testing.T) {
	mod := func(f func(*domain.TacheRemux)) domain.TacheRemux { tc := tache(); f(&tc); return tc }
	cas := map[string]domain.TacheRemux{
		"url vide":           mod(func(t *domain.TacheRemux) { t.URLAmont = "" }),
		"fichier local":      mod(func(t *domain.TacheRemux) { t.URLAmont = "file:///etc/passwd" }),
		"option":             mod(func(t *domain.TacheRemux) { t.URLAmont = "-i http://x/y" }),
		"chemin nu":          mod(func(t *domain.TacheRemux) { t.URLAmont = "/etc/passwd" }),
		"concat":             mod(func(t *domain.TacheRemux) { t.URLAmont = "concat:a|b" }),
		"espace":             mod(func(t *domain.TacheRemux) { t.URLAmont = "http://x.test/a b" }),
		"saut de ligne":      mod(func(t *domain.TacheRemux) { t.URLAmont = "http://x.test/a\n-f" }),
		"sans hote":          mod(func(t *domain.TacheRemux) { t.URLAmont = "http:///x" }),
		"ftp":                mod(func(t *domain.TacheRemux) { t.URLAmont = "ftp://x.test/a" }),
		"url trop longue":    mod(func(t *domain.TacheRemux) { t.URLAmont = "http://x.test/" + strings.Repeat("a", 3000) }),
		"entete injecte":     mod(func(t *domain.TacheRemux) { t.Entetes = http.Header{"Cookie": {"a=b"}} }),
		"agent multiligne":   mod(func(t *domain.TacheRemux) { t.Entetes = http.Header{"User-Agent": {"a\r\nX: y"}} }),
		"agent trop long":    mod(func(t *domain.TacheRemux) { t.Entetes = http.Header{"User-Agent": {strings.Repeat("a", 400)}} }),
		"position negative":  mod(func(t *domain.TacheRemux) { t.Debut = -time.Second }),
		"position delirante": mod(func(t *domain.TacheRemux) { t.Debut = 1000 * time.Hour }),
		"mode direct":        mod(func(t *domain.TacheRemux) { t.Mode = domain.ModeDirect }),
		"mode inconnu":       mod(func(t *domain.TacheRemux) { t.Mode = "x -f lavfi" }),
	}
	for nom, tc := range cas {
		if _, err := Arguments(tc); !errors.Is(err, ErrArgumentRefuse) {
			t.Errorf("%s : erreur = %v, attendu ErrArgumentRefuse", nom, err)
		}
	}
	// Une URL pleine de metacaracteres de shell reste UN SEUL argument (aucun shell).
	tc := tache()
	tc.URLAmont = "http://x.test/a;rm$(id)&b|c`d`?q=1&r=2"
	a, err := Arguments(tc)
	if err != nil {
		t.Fatal(err)
	}
	if !contient(a, "-i", tc.URLAmont) {
		t.Errorf("l'URL n'est pas un argument unique : %v", a)
	}
}

// ---------------------------------------------------------------- sonde

const sortieMkv = `{"streams":[
 {"codec_type":"video","codec_name":"mjpeg"},
 {"codec_type":"video","codec_name":"h264","height":1080},
 {"codec_type":"audio","codec_name":"eac3"},
 {"codec_type":"audio","codec_name":"aac"},
 {"codec_type":"subtitle","codec_name":"subrip"}],
 "format":{"format_name":"matroska,webm","duration":"5487.200000"}}`

func TestAnalyserSondeIgnoreLesPochettes(t *testing.T) {
	s, err := AnalyserSonde([]byte(sortieMkv))
	if err != nil {
		t.Fatal(err)
	}
	if s.CodecVideo != "h264" || s.CodecAudio != "eac3" || s.Conteneur != "matroska,webm" {
		t.Errorf("sonde = %+v", s)
	}
	if s.Duree != 5487200*time.Millisecond {
		t.Errorf("duree = %v", s.Duree)
	}
	if _, err := AnalyserSonde([]byte("pas du json")); err == nil {
		t.Error("json invalide accepte")
	}
	if _, err := AnalyserSonde([]byte(`{"streams":[],"format":{}}`)); err == nil {
		t.Error("fichier sans flux accepte")
	}
}

// exeFaux double l'Executeur.
type exeFaux struct {
	mu        sync.Mutex
	executees [][]string
	sortie    string
	err       error
	bloque    chan struct{} // si non nil : Executer attend sa fermeture
	demarrer  func(ctx context.Context, nom string, args []string) (domain.Processus, error)
}

func (e *exeFaux) Executer(ctx context.Context, nom string, args []string, max int) ([]byte, error) {
	e.mu.Lock()
	e.executees = append(e.executees, append([]string{nom}, args...))
	e.mu.Unlock()
	if e.bloque != nil {
		select {
		case <-e.bloque:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if e.err != nil {
		return nil, e.err
	}
	return []byte(e.sortie), nil
}

func (e *exeFaux) Demarrer(ctx context.Context, nom string, args []string) (domain.Processus, error) {
	return e.demarrer(ctx, nom, args)
}

func (e *exeFaux) appels() int { e.mu.Lock(); defer e.mu.Unlock(); return len(e.executees) }

type horlogeFixe struct {
	mu sync.Mutex
	t  time.Time
}

func (h *horlogeFixe) now() time.Time { h.mu.Lock(); defer h.mu.Unlock(); return h.t }
func (h *horlogeFixe) avance(d time.Duration) {
	h.mu.Lock()
	h.t = h.t.Add(d)
	h.mu.Unlock()
}

const urlFilm = "http://fournisseur.test/movie/utilisateur/secret123/1.mkv"

// CA2 : la sonde est mise en cache pendant duree_sonde, puis refaite.
func TestSondeMiseEnCache(t *testing.T) {
	exe := &exeFaux{sortie: sortieMkv}
	h := &horlogeFixe{t: time.Unix(1000, 0)}
	s := NouveauSondeur(ReglagesSonde{Ffprobe: "ffprobe", Duree: time.Hour, Delai: time.Second, Max: 10}, exe, h.now)
	for i := 0; i < 3; i++ {
		r, err := s.Sonder(context.Background(), urlFilm, nil)
		if err != nil || r.CodecAudio != "eac3" {
			t.Fatalf("sonde %d : %v %+v", i, err, r)
		}
	}
	if exe.appels() != 1 {
		t.Errorf("ffprobe lance %d fois, attendu 1", exe.appels())
	}
	h.avance(59 * time.Minute)
	s.Sonder(context.Background(), urlFilm, nil)
	if exe.appels() != 1 {
		t.Errorf("sonde refaite avant l'expiration")
	}
	h.avance(2 * time.Minute)
	s.Sonder(context.Background(), urlFilm, nil)
	if exe.appels() != 2 {
		t.Errorf("sonde non refaite apres l'expiration : %d appels", exe.appels())
	}
	// Une autre URL est une autre sonde.
	s.Sonder(context.Background(), urlFilm+"x", nil)
	if exe.appels() != 3 {
		t.Errorf("appels = %d", exe.appels())
	}
}

// CA2 : N demandes simultanees = un seul ffprobe (une seule connexion fournisseur).
func TestSondeUneSeuleExecutionPourDesDemandesSimultanees(t *testing.T) {
	exe := &exeFaux{sortie: sortieMkv, bloque: make(chan struct{})}
	s := NouveauSondeur(ReglagesSonde{Ffprobe: "ffprobe", Duree: time.Hour, Delai: 5 * time.Second, Max: 10}, exe, nil)
	var wg sync.WaitGroup
	var echecs atomic.Int32
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Sonder(context.Background(), urlFilm, nil); err != nil {
				echecs.Add(1)
			}
		}()
	}
	for exe.appels() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(30 * time.Millisecond)
	close(exe.bloque)
	wg.Wait()
	if exe.appels() != 1 || echecs.Load() != 0 {
		t.Errorf("appels = %d, echecs = %d", exe.appels(), echecs.Load())
	}
}

func TestSondeAnnuleeParLeContexte(t *testing.T) {
	exe := &exeFaux{sortie: sortieMkv, bloque: make(chan struct{})}
	s := NouveauSondeur(ReglagesSonde{Ffprobe: "ffprobe", Duree: time.Hour, Delai: 5 * time.Second, Max: 10}, exe, nil)
	ctx, annule := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); annule() }()
	if _, err := s.Sonder(ctx, urlFilm, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("erreur = %v", err)
	}
	// L'echec n'est pas mis en cache.
	exe.bloque = nil
	if _, err := s.Sonder(context.Background(), urlFilm, nil); err != nil {
		t.Errorf("sonde suivante : %v", err)
	}
}

func TestSondeDelaiMaximal(t *testing.T) {
	exe := &exeFaux{sortie: sortieMkv, bloque: make(chan struct{})}
	defer close(exe.bloque)
	s := NouveauSondeur(ReglagesSonde{Ffprobe: "ffprobe", Duree: time.Hour, Delai: 40 * time.Millisecond, Max: 10}, exe, nil)
	debut := time.Now()
	if _, err := s.Sonder(context.Background(), urlFilm, nil); err == nil {
		t.Fatal("pas d'erreur")
	}
	if time.Since(debut) > 2*time.Second {
		t.Error("delai_sonde non respecte")
	}
}

// Le cache est borne, et les echecs et refus ne contiennent pas l'URL.
func TestSondeCacheBorneEtErreursSansURL(t *testing.T) {
	exe := &exeFaux{sortie: sortieMkv}
	h := &horlogeFixe{t: time.Unix(1000, 0)}
	s := NouveauSondeur(ReglagesSonde{Ffprobe: "ffprobe", Duree: time.Hour, Max: 3}, exe, h.now)
	for i := 0; i < 10; i++ {
		h.avance(time.Second)
		if _, err := s.Sonder(context.Background(), urlFilm+strings.Repeat("x", i), nil); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.cache) != 3 {
		t.Errorf("cache a %d entrees, borne 3", len(s.cache))
	}
	// La plus recente est encore la.
	n := exe.appels()
	s.Sonder(context.Background(), urlFilm+strings.Repeat("x", 9), nil)
	if exe.appels() != n {
		t.Error("l'entree la plus recente a ete evincee")
	}
	_, err := s.Sonder(context.Background(), "file:///etc/passwd", nil)
	if !errors.Is(err, ErrArgumentRefuse) {
		t.Errorf("url locale : %v", err)
	}
	exe.err = errors.New("ffprobe : exit status 1")
	_, err = s.Sonder(context.Background(), urlFilm+"autre", nil)
	if err == nil || strings.Contains(err.Error(), "secret123") {
		t.Errorf("erreur = %v", err)
	}
}

// ---------------------------------------------------------------- lanceur

// procFaux est un processus qui vit jusqu'a Arreter.
type procFaux struct {
	arrete chan struct{}
	once   sync.Once
	nArret atomic.Int32
}

func nouveauProc() *procFaux              { return &procFaux{arrete: make(chan struct{})} }
func (p *procFaux) Sortie() io.ReadCloser { return io.NopCloser(strings.NewReader("")) }
func (p *procFaux) Arreter()              { p.nArret.Add(1); p.once.Do(func() { close(p.arrete) }) }
func (p *procFaux) Attendre() error       { <-p.arrete; return nil }

type usine struct {
	mu    sync.Mutex
	procs []*procFaux
	ctxs  []context.Context
	args  [][]string
}

func (u *usine) demarrer(ctx context.Context, nom string, args []string) (domain.Processus, error) {
	p := nouveauProc()
	u.mu.Lock()
	u.procs = append(u.procs, p)
	u.ctxs = append(u.ctxs, ctx)
	u.args = append(u.args, args)
	u.mu.Unlock()
	go func() { // comme le vrai : l'annulation du contexte tue le processus
		select {
		case <-ctx.Done():
			p.Arreter()
		case <-p.arrete:
		}
	}()
	return p, nil
}

func (u *usine) nombre() int { u.mu.Lock(); defer u.mu.Unlock(); return len(u.procs) }

func attendre(t *testing.T, cond func() bool, quoi string) {
	t.Helper()
	fin := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(fin) {
			t.Fatalf("delai depasse : %s", quoi)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func lanceurDe(t *testing.T, max, file int, attente time.Duration) (*Lanceur, *usine) {
	u := &usine{}
	l, err := NouveauLanceur(Reglages{Ffmpeg: "ffmpeg", ProcessusMax: max, FileMax: file, AttenteFile: attente},
		&exeFaux{demarrer: u.demarrer})
	if err != nil {
		t.Fatal(err)
	}
	return l, u
}

// CA3 : jamais plus de processus_max processus ; la file est bornee ; un emplacement
// rendu fait avancer la file.
func TestLanceurRespecteProcessusMaxEtLaFile(t *testing.T) {
	l, u := lanceurDe(t, 2, 1, 2*time.Second)
	ctx := context.Background()
	p1, err := l.Lancer(ctx, tache())
	if err != nil {
		t.Fatal(err)
	}
	p2, err := l.Lancer(ctx, tache())
	if err != nil {
		t.Fatal(err)
	}
	// Troisieme : attend dans la file.
	type res struct {
		p   domain.Processus
		err error
	}
	c3 := make(chan res, 1)
	go func() { p, err := l.Lancer(ctx, tache()); c3 <- res{p, err} }()
	attendre(t, func() bool { return l.EnAttente() == 1 }, "troisieme en file")
	// Quatrieme : la file (1) est pleine, refus immediat.
	if _, err := l.Lancer(ctx, tache()); !errors.Is(err, ErrSaturation) {
		t.Fatalf("quatrieme : %v, attendu ErrSaturation", err)
	}
	if u.nombre() != 2 {
		t.Fatalf("%d processus alors que le maximum est 2", u.nombre())
	}
	p1.Arreter()
	r := <-c3
	if r.err != nil {
		t.Fatalf("troisieme : %v", r.err)
	}
	if u.nombre() != 3 || l.EnCours() != 2 {
		t.Errorf("processus lances %d, en cours %d", u.nombre(), l.EnCours())
	}
	p2.Arreter()
	r.p.Arreter()
	attendre(t, func() bool { return l.EnCours() == 0 }, "emplacements rendus")
}

func TestLanceurAttenteDepassee(t *testing.T) {
	l, _ := lanceurDe(t, 1, 2, 30*time.Millisecond)
	p, _ := l.Lancer(context.Background(), tache())
	defer p.Arreter()
	if _, err := l.Lancer(context.Background(), tache()); !errors.Is(err, ErrSaturation) {
		t.Errorf("erreur = %v", err)
	}
	if l.EnAttente() != 0 {
		t.Errorf("file non videe : %d", l.EnAttente())
	}
}

func TestLanceurSansFileRefuseTout(t *testing.T) {
	l, _ := lanceurDe(t, 1, 0, time.Second)
	p, _ := l.Lancer(context.Background(), tache())
	defer p.Arreter()
	if _, err := l.Lancer(context.Background(), tache()); !errors.Is(err, ErrSaturation) {
		t.Errorf("erreur = %v", err)
	}
}

func TestLanceurAnnulationPendantLAttente(t *testing.T) {
	l, _ := lanceurDe(t, 1, 2, time.Minute)
	p, _ := l.Lancer(context.Background(), tache())
	defer p.Arreter()
	ctx, annule := context.WithCancel(context.Background())
	c := make(chan error, 1)
	go func() { _, err := l.Lancer(ctx, tache()); c <- err }()
	attendre(t, func() bool { return l.EnAttente() == 1 }, "en file")
	annule()
	if err := <-c; !errors.Is(err, context.Canceled) {
		t.Errorf("erreur = %v", err)
	}
	if l.EnAttente() != 0 {
		t.Error("file non vidée")
	}
}

// CA3 : le client parti (contexte annule) tue le processus et rend l'emplacement.
func TestLanceurLeContexteAnnuleTueLeProcessus(t *testing.T) {
	l, u := lanceurDe(t, 1, 0, time.Second)
	ctx, annule := context.WithCancel(context.Background())
	p, err := l.Lancer(ctx, tache())
	if err != nil {
		t.Fatal(err)
	}
	annule()
	attendre(t, func() bool { return u.procs[0].nArret.Load() > 0 && l.EnCours() == 0 }, "processus arrete")
	_ = p.Attendre()
	// L'emplacement est reutilisable.
	p2, err := l.Lancer(context.Background(), tache())
	if err != nil {
		t.Fatal(err)
	}
	p2.Arreter()
	p2.Arreter() // idempotent
}

func TestLanceurRefuseUneTacheInvalideSansRienLancer(t *testing.T) {
	l, u := lanceurDe(t, 1, 0, time.Second)
	tc := tache()
	tc.URLAmont = "file:///etc/passwd"
	if _, err := l.Lancer(context.Background(), tc); !errors.Is(err, ErrArgumentRefuse) {
		t.Errorf("erreur = %v", err)
	}
	if u.nombre() != 0 || l.EnCours() != 0 {
		t.Error("un processus a ete lance")
	}
}

func TestLanceurEchecDeDemarrageRendLEmplacement(t *testing.T) {
	exe := &exeFaux{demarrer: func(context.Context, string, []string) (domain.Processus, error) {
		return nil, errors.New("introuvable")
	}}
	l, _ := NouveauLanceur(Reglages{Ffmpeg: "ffmpeg", ProcessusMax: 1}, exe)
	if _, err := l.Lancer(context.Background(), tache()); err == nil {
		t.Fatal("pas d'erreur")
	}
	if l.EnCours() != 0 {
		t.Error("emplacement perdu")
	}
}

func TestNouveauLanceurValide(t *testing.T) {
	if _, err := NouveauLanceur(Reglages{Ffmpeg: "ffmpeg"}, nil); err == nil {
		t.Error("processus_max 0 accepte")
	}
	if _, err := NouveauLanceur(Reglages{ProcessusMax: 1}, nil); err == nil {
		t.Error("ffmpeg vide accepte")
	}
}
