package live

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"mytv/internal/config"
	"mytv/internal/domain"
	"mytv/internal/remux"
	"mytv/internal/testutil/fauxiptv"
)

var reSegmentOpaque = regexp.MustCompile(`^/api/v1/live/201/seg/[A-Za-z0-9_-]+\.ts$`)

func (b *bancConversion) playlist(chaine, session, requete string) *httptest.ResponseRecorder {
	return b.requete("/api/v1/live/"+chaine+".m3u8"+requete, session, nil)
}

func (b *bancConversion) aucunResidu(t *testing.T) {
	t.Helper()
	attendreQue(t, 5*time.Second, func() bool { return processusDe(b.dossier) == 0 && len(sousDossiers(b.dossier)) == 0 },
		"zero ffmpeg et zero dossier restant")
}

func (b *bancConversion) corpsErreur(t *testing.T, rec *httptest.ResponseRecorder) (code string) {
	t.Helper()
	var c struct{ Code, Message string }
	if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil {
		t.Fatalf("corps non JSON : %q", rec.Body.String())
	}
	if rec.Header().Get("X-Mytv-Erreur") != c.Code {
		t.Errorf("X-Mytv-Erreur = %q, code = %q", rec.Header().Get("X-Mytv-Erreur"), c.Code)
	}
	return c.Code
}

// ---------------------------------------------------------------- US-30

// US-30 CA1, CA3 : une chaine h264+aac est sondee UNE fois (ffprobe lit une source locale, jamais
// l'adresse du fournisseur), servie en direct sans aucun ffmpeg, et le mode est annonce.
func TestSondeDirectH264AACPuisCache(t *testing.T) {
	b := nouveauBancConversion(t, optionsBanc{formats: map[string]formatTest{"101": formatH264AAC}})
	rec := b.playlist("101", "s1", "")
	if rec.Code != 200 || rec.Header().Get("X-Mytv-Mode") != "direct" || rec.Header().Get("X-Mytv-Codec") != "h264+aac" || rec.Header().Get("X-Mytv-Hauteur") != "360" {
		t.Fatalf("HTTP %d, en-tetes %v", rec.Code, rec.Header())
	}
	if n := len(b.exe.appels("ffprobe")); n != 1 {
		t.Fatalf("ffprobe lance %d fois", n)
	}
	// Deuxieme demande : verdict en cache, aucune sonde de plus, aucun ffmpeg.
	for i := 0; i < 3; i++ {
		if r := b.playlist("101", "s1", ""); r.Code != 200 || r.Header().Get("X-Mytv-Mode") != "direct" {
			t.Fatalf("HTTP %d", r.Code)
		}
	}
	if n := len(b.exe.appels("ffprobe")); n != 1 {
		t.Errorf("ffprobe relance : %d appels", n)
	}
	if n := len(b.exe.appels("ffmpeg")); n != 0 {
		t.Errorf("ffmpeg lance pour une chaine lisible : %d", n)
	}
	verifierArgv(t, b.exe.appels("ff"))
	b.verifierSecrets(t, rec.Body.String())
	// Le segment annonce se relaie comme avant.
	seg := lignesSegments(rec.Body.String())[0]
	if s := b.requete(seg, "s1", nil); s.Code != 200 || s.Body.Len() < 1000 {
		t.Errorf("segment : HTTP %d, %d octets", s.Code, s.Body.Len())
	}
}

// US-30 CA1 : singleflight par chaine, une seule sonde a la fois.
func TestSondeUneSeuleParChaineEtParMoment(t *testing.T) {
	var enCours, pic, appels atomic.Int32
	debloquer := make(chan struct{})
	faux := &sondeurScripte{f: func(ctx context.Context, src domain.SourceOctets) (remux.SondeDirect, error) {
		appels.Add(1)
		n := enCours.Add(1)
		for {
			p := pic.Load()
			if n <= p || pic.CompareAndSwap(p, n) {
				break
			}
		}
		defer enCours.Add(-1)
		select {
		case <-debloquer:
		case <-ctx.Done():
			return remux.SondeDirect{}, ctx.Err()
		}
		return remux.SondeDirect{CodecVideo: "h264", Hauteur: 720, CodecAudio: "aac"}, nil
	}}
	b := nouveauBancConversion(t, optionsBanc{formats: map[string]formatTest{"101": formatH264AAC, "102": formatH264AAC},
		ajuste: func(o *Options) { o.Sondeur = faux }})
	var wg sync.WaitGroup
	codes := make(chan int, 8)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); codes <- b.playlist("101", "s1", "").Code }()
	}
	attendreQue(t, 5*time.Second, func() bool { return appels.Load() >= 1 }, "premiere sonde")
	time.Sleep(150 * time.Millisecond)
	close(debloquer)
	wg.Wait()
	close(codes)
	for c := range codes {
		if c != 200 {
			t.Errorf("HTTP %d", c)
		}
	}
	if appels.Load() != 1 {
		t.Errorf("%d sondes pour 4 demandes simultanees de la meme chaine", appels.Load())
	}
	if pic.Load() != 1 {
		t.Errorf("pic de sondes simultanees = %d", pic.Load())
	}
}

type sondeurScripte struct {
	f func(ctx context.Context, src domain.SourceOctets) (remux.SondeDirect, error)
}

func (s *sondeurScripte) SonderDirect(ctx context.Context, src domain.SourceOctets) (remux.SondeDirect, error) {
	return s.f(ctx, src)
}

// US-30 CA1 : une sonde qui echoue ou depasse son delai n'interdit pas la lecture (relais brut),
// et n'est pas retentee a chaque rechargement de playlist.
func TestSondeEchecRepliSurLeRelaisBrutSansRetenter(t *testing.T) {
	var appels atomic.Int32
	faux := &sondeurScripte{f: func(ctx context.Context, src domain.SourceOctets) (remux.SondeDirect, error) {
		appels.Add(1)
		<-ctx.Done() // ne repond jamais : seul le delai de la sonde y met fin
		return remux.SondeDirect{}, ctx.Err()
	}}
	b := nouveauBancConversion(t, optionsBanc{formats: map[string]formatTest{"101": formatH264AAC},
		ajuste: func(o *Options) {
			o.Sondeur = faux
			o.Live.DelaiSonde = config.Duree(200 * time.Millisecond)
		}})
	debut := time.Now()
	rec := b.playlist("101", "s1", "")
	if rec.Code != 200 || rec.Header().Get("X-Mytv-Mode") != "direct" {
		t.Fatalf("HTTP %d mode %q", rec.Code, rec.Header().Get("X-Mytv-Mode"))
	}
	if d := time.Since(debut); d > 3*time.Second {
		t.Errorf("la premiere lecture a attendu %v malgre un delai de sonde de 200ms", d)
	}
	if rec.Header().Get("X-Mytv-Codec") != "" {
		t.Errorf("codec annonce sans sonde : %q", rec.Header().Get("X-Mytv-Codec"))
	}
	debut = time.Now()
	for i := 0; i < 3; i++ {
		b.playlist("101", "s1", "")
	}
	if appels.Load() != 1 {
		t.Errorf("%d sondes : l'echec doit etre memorise (live.echec_sonde)", appels.Load())
	}
	if time.Since(debut) > time.Second {
		t.Errorf("rechargements ralentis par une sonde en echec")
	}
}

// US-30 CA1 : un segment illisible (ffprobe echoue pour de vrai) laisse lire la chaine.
func TestSondeSurSegmentIllisibleLaisseLire(t *testing.T) {
	b := nouveauBancConversion(t, optionsBanc{formats: map[string]formatTest{"101": formatH264AAC}, faux: func(o *fauxiptv.Options) {
		o.ContenuSegment = func(string) ([]byte, bool) { return []byte("ceci n'est pas du MPEG-TS"), true }
	}})
	rec := b.playlist("101", "s1", "")
	if rec.Code != 200 || rec.Header().Get("X-Mytv-Mode") != "direct" {
		t.Fatalf("HTTP %d mode %q", rec.Code, rec.Header().Get("X-Mytv-Mode"))
	}
}

// US-30 CA1 : un verdict deja connu n'ajoute aucun delai ; un verdict PERIME est servi tout de
// suite et la sonde repart derriere.
func TestVerdictPerimeServiSansAttendreEtResonde(t *testing.T) {
	var appels atomic.Int32
	lent := make(chan struct{})
	faux := &sondeurScripte{f: func(ctx context.Context, src domain.SourceOctets) (remux.SondeDirect, error) {
		if appels.Add(1) > 1 {
			select { // les resondes sont lentes
			case <-lent:
			case <-ctx.Done():
			}
		}
		return remux.SondeDirect{CodecVideo: "h264", Hauteur: 720, CodecAudio: "aac"}, nil
	}}
	var maintenant atomic.Int64
	maintenant.Store(time.Now().UnixNano())
	b := nouveauBancConversion(t, optionsBanc{formats: map[string]formatTest{"101": formatH264AAC},
		ajuste: func(o *Options) {
			o.Sondeur = faux
			o.Horloge = func() time.Time { return time.Unix(0, maintenant.Load()) }
		}})
	defer close(lent)
	if r := b.playlist("101", "s1", ""); r.Code != 200 {
		t.Fatal(r.Code)
	}
	maintenant.Add(int64(11 * time.Minute)) // duree_sonde = 10 min
	debut := time.Now()
	rec := b.playlist("101", "s1", "")
	if rec.Code != 200 || rec.Header().Get("X-Mytv-Mode") != "direct" || time.Since(debut) > time.Second {
		t.Fatalf("HTTP %d mode %q en %v", rec.Code, rec.Header().Get("X-Mytv-Mode"), time.Since(debut))
	}
	attendreQue(t, 3*time.Second, func() bool { return appels.Load() == 2 }, "nouvelle sonde en arriere-plan")
}

// US-30 CA2, CA3 : HEVC 4K + EAC3, navigateur incapable (aucun en-tete) : refus 415 net, nomme
// le codec et la hauteur, ne lance AUCUN ffmpeg.
func TestHEVC4KRefuseSansCapacites(t *testing.T) {
	b := nouveauBancConversion(t, optionsBanc{formats: map[string]formatTest{"4k": formatHEVC4K}})
	rec := b.playlist("4k", "s1", "")
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("HTTP %d : %s", rec.Code, rec.Body.String())
	}
	if code := b.corpsErreur(t, rec); code != "format_hevc_non_lisible" {
		t.Errorf("code = %q", code)
	}
	if rec.Header().Get("X-Mytv-Mode") != "refus" || rec.Header().Get("X-Mytv-Codec") != "hevc+eac3" || rec.Header().Get("X-Mytv-Hauteur") != "2160" {
		t.Errorf("en-tetes : %v", rec.Header())
	}
	if !strings.Contains(rec.Body.String(), "hevc") || !strings.Contains(rec.Body.String(), "2160") {
		t.Errorf("message sans codec ni hauteur : %s", rec.Body.String())
	}
	if len(b.exe.appels("ffmpeg")) != 0 || processusDe(b.dossier) != 0 {
		t.Error("ffmpeg lance pour un refus")
	}
	// Meme avec un navigateur capable d'EAC3 seulement : sans HEVC, c'est un refus.
	if r := b.playlist("4k", "s1", "?cap=eac3"); r.Code != 415 {
		t.Errorf("capable eac3 seulement : HTTP %d", r.Code)
	}
}

// US-31 CA6 (et US-30 CA5) : la meme 4K HEVC+EAC3 pour un navigateur capable passe par le relais
// d'origine : 200, mode direct, octets du fournisseur inchanges, ZERO processus ffmpeg.
func TestHEVC4KJoueEnDirectPourUnNavigateurCapable(t *testing.T) {
	b := nouveauBancConversion(t, optionsBanc{formats: map[string]formatTest{"4k": formatHEVC4K}})
	rec := b.requete("/api/v1/live/4k.m3u8", "s1", map[string]string{"X-Mytv-Capacites": "hevc,eac3"})
	if rec.Code != 200 || rec.Header().Get("X-Mytv-Mode") != "direct" || rec.Header().Get("X-Mytv-Hauteur") != "2160" {
		t.Fatalf("HTTP %d %v %s", rec.Code, rec.Header(), rec.Body.String())
	}
	seg := b.recupererSegment(t, lignesSegments(rec.Body.String())[0], "s1")
	s, _ := b.sondeFichier(t, seg)
	if s.CodecVideo != "hevc" || s.Hauteur != 2160 || s.CodecAudio != "eac3" {
		t.Errorf("le segment relaye a ete modifie : %+v", s)
	}
	// Le parametre ?cap= vaut l'en-tete.
	if r := b.playlist("4k", "s1", "?cap=hevc,eac3"); r.Code != 200 || r.Header().Get("X-Mytv-Mode") != "direct" {
		t.Errorf("?cap= : HTTP %d mode %q", r.Code, r.Header().Get("X-Mytv-Mode"))
	}
	if len(b.exe.appels("ffmpeg")) != 0 || processusDe(b.dossier) != 0 {
		t.Error("CA6 : un ffmpeg a ete lance pour un navigateur capable")
	}
	if e := sousDossiers(b.dossier); len(e) != 0 {
		t.Errorf("dossier temporaire cree sans transcodage : %v", e)
	}
}

// US-30 CA4 : ?forcer= n'accepte que audio et video.
func TestForcerRefuseHorListe(t *testing.T) {
	b := nouveauBancConversion(t, optionsBanc{formats: map[string]formatTest{"101": formatH264AAC}})
	for _, v := range []string{"direct", "refus", "x", "AUDIO", "audio,video", "%00"} {
		rec := b.playlist("101", "s1", "?forcer="+v)
		if rec.Code != 400 || b.corpsErreur(t, rec) != "forcer_invalide" {
			t.Errorf("forcer=%q : HTTP %d", v, rec.Code)
		}
	}
	if b.baux.Occupes("iptva") != 0 {
		t.Error("une demande invalide a reserve la connexion du fournisseur")
	}
	if len(b.exe.appels("ff")) != 0 {
		t.Error("programme lance pour une demande invalide")
	}
}

// ---------------------------------------------------------------- US-31

// US-31 CA1, CA2, CA4 : HEVC 640x360 + EAC3 -> mode video. ffmpeg produit un HLS dont les segments
// (verifies par ffprobe) sont h264 + aac stereo, l'URL du fournisseur et les identifiants n'apparaissent
// ni en argv ni dans les reponses, les URL de segments sont stables d'une playlist a l'autre.
func TestTranscodageVideoHEVCEAC3(t *testing.T) {
	b := nouveauBancConversion(t, optionsBanc{formats: map[string]formatTest{"201": formatHEVCEAC3}})
	rec := b.playlist("201", "s1", "")
	if rec.Code != 200 {
		t.Fatalf("HTTP %d : %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Mytv-Mode") != "video" || rec.Header().Get("X-Mytv-Codec") != "" && rec.Header().Get("X-Mytv-Codec") != "hevc+eac3" {
		t.Errorf("en-tetes : %v", rec.Header())
	}
	segs := lignesSegments(rec.Body.String())
	if len(segs) < 2 || !strings.HasPrefix(segs[0], "/api/v1/live/201/seg/") {
		t.Fatalf("playlist : %q", rec.Body.String())
	}
	if n := processusDe(b.dossier); n != 1 {
		t.Errorf("%d ffmpeg vivants", n)
	}
	corps := b.recupererSegment(t, segs[0], "s1")
	s, canaux := b.sondeFichier(t, corps)
	if s.CodecVideo != "h264" || s.CodecAudio != "aac" || canaux != 2 || s.Hauteur != 360 {
		t.Errorf("sortie = %+v, %d canaux (attendu h264 360p + aac stereo)", s, canaux)
	}
	// Les URL de segments sont des jetons opaques, stables (le meme fichier donne le meme jeton).
	rec2 := b.playlist("201", "s1", "")
	segs2 := lignesSegments(rec2.Body.String())
	for _, u := range append(append([]string{}, segs...), segs2...) {
		if !reSegmentOpaque.MatchString(u) {
			t.Errorf("URL de segment inattendue : %s", u)
		}
	}
	// Argv : aucune URL fournisseur, aucun identifiant.
	ff := b.exe.appels("ffmpeg")
	if len(ff) != 1 {
		t.Fatalf("%d lancements de ffmpeg", len(ff))
	}
	verifierArgv(t, b.exe.appels("ff"))
	if !strings.Contains(strings.Join(ff[0].args, " "), "pipe:0") || !strings.Contains(strings.Join(ff[0].args, " "), "libx264") {
		t.Errorf("argv : %v", ff[0].args)
	}
	b.verifierSecrets(t, rec.Body.String(), rec2.Body.String())
	// Aucune connexion du fournisseur ne reste ouverte apres la fin de lecture.
	b.relais.FinDeLecture("s1", "201")
	b.aucunResidu(t)
}

// US-31 CA4 : h264 + AC3 -> mode audio, la video est COPIEE, l'audio devient aac stereo.
func TestTranscodageAudioH264AC3(t *testing.T) {
	b := nouveauBancConversion(t, optionsBanc{formats: map[string]formatTest{"202": formatH264AC3}})
	rec := b.playlist("202", "s1", "")
	if rec.Code != 200 || rec.Header().Get("X-Mytv-Mode") != "audio" {
		t.Fatalf("HTTP %d mode %q : %s", rec.Code, rec.Header().Get("X-Mytv-Mode"), rec.Body.String())
	}
	corps := b.recupererSegment(t, lignesSegments(rec.Body.String())[0], "s1")
	s, canaux := b.sondeFichier(t, corps)
	if s.CodecVideo != "h264" || s.CodecAudio != "aac" || canaux != 2 {
		t.Errorf("sortie = %+v, %d canaux", s, canaux)
	}
	args := strings.Join(b.exe.appels("ffmpeg")[0].args, " ")
	if !strings.Contains(args, "-c:v copy") || strings.Contains(args, "libx264") {
		t.Errorf("la video doit etre copiee : %s", args)
	}
	b.relais.FinDeLecture("s1", "202")
	b.aucunResidu(t)
}

// US-31 CA4 : une chaine h264 + aac ne lance jamais ffmpeg (aucune regression de performance).
func TestChaineLisibleNeLanceAucunFfmpeg(t *testing.T) {
	b := nouveauBancConversion(t, optionsBanc{formats: map[string]formatTest{"203": formatH264AAC}})
	for i := 0; i < 3; i++ {
		if r := b.playlist("203", "s1", ""); r.Code != 200 || r.Header().Get("X-Mytv-Mode") != "direct" {
			t.Fatalf("HTTP %d", r.Code)
		}
	}
	if len(b.exe.appels("ffmpeg")) != 0 || len(sousDossiers(b.dossier)) != 0 {
		t.Error("ffmpeg ou dossier pour une chaine lisible")
	}
}

// US-31 CA2 : zapping. L'ancien ffmpeg est arrete et son dossier supprime AVANT que la nouvelle
// playlist soit rendue ; un seul ffmpeg vivant a tout moment ; direct -> converti -> direct.
func TestZappingUnSeulFfmpegEtDossiersNettoyes(t *testing.T) {
	b := nouveauBancConversion(t, optionsBanc{formats: map[string]formatTest{"201": formatHEVCEAC3, "202": formatH264AC3, "203": formatH264AAC}})
	if r := b.playlist("201", "s1", ""); r.Code != 200 || r.Header().Get("X-Mytv-Mode") != "video" {
		t.Fatalf("201 : HTTP %d", r.Code)
	}
	d1 := sousDossiers(b.dossier)
	if len(d1) != 1 || !strings.HasPrefix(d1[0], "201-") {
		t.Fatalf("dossiers = %v", d1)
	}
	if r := b.playlist("202", "s1", ""); r.Code != 200 || r.Header().Get("X-Mytv-Mode") != "audio" {
		t.Fatalf("202 : HTTP %d", r.Code)
	}
	if d := sousDossiers(b.dossier); len(d) != 1 || !strings.HasPrefix(d[0], "202-") {
		t.Errorf("l'ancien dossier subsiste : %v", d)
	}
	if n := processusDe(b.dossier); n != 1 {
		t.Errorf("%d ffmpeg apres zapping", n)
	}
	// Vers une chaine lisible : plus aucun ffmpeg.
	if r := b.playlist("203", "s1", ""); r.Code != 200 || r.Header().Get("X-Mytv-Mode") != "direct" {
		t.Fatalf("203 : HTTP %d", r.Code)
	}
	b.aucunResidu(t)
	// Un jeton de l'ancienne chaine ne sert plus.
	if len(b.exe.appels("ffmpeg")) != 2 {
		t.Errorf("%d lancements de ffmpeg", len(b.exe.appels("ffmpeg")))
	}
}

// US-31 CA2 : une demande ne lance qu'un ffmpeg meme si plusieurs lecteurs/rechargements arrivent
// pendant le demarrage.
func TestDemandesSimultanees_UnSeulFfmpeg(t *testing.T) {
	b := nouveauBancConversion(t, optionsBanc{formats: map[string]formatTest{"201": formatHEVCEAC3}})
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r := b.playlist("201", "s1", ""); r.Code != 200 {
				t.Errorf("HTTP %d", r.Code)
			}
		}()
	}
	wg.Wait()
	if n := len(b.exe.appels("ffmpeg")); n != 1 {
		t.Errorf("%d ffmpeg lances", n)
	}
}

// US-31 CA2 : POST /live/{chaine}/fin -> processus tue, dossier supprime, bail rendu.
func TestFinDeLectureArreteFfmpeg(t *testing.T) {
	b := nouveauBancConversion(t, optionsBanc{formats: map[string]formatTest{"201": formatHEVCEAC3}})
	if r := b.playlist("201", "s1", ""); r.Code != 200 {
		t.Fatal(r.Code)
	}
	// Un appel tardif d'une AUTRE chaine ne coupe rien.
	b.relais.FinDeLecture("s1", "999")
	if processusDe(b.dossier) != 1 {
		t.Fatal("ffmpeg coupe par la fin d'une autre chaine")
	}
	// Une autre session non plus.
	b.relais.FinDeLecture("s2", "201")
	if processusDe(b.dossier) != 1 {
		t.Fatal("ffmpeg coupe par une autre session")
	}
	b.relais.FinDeLecture("s1", "201")
	b.aucunResidu(t)
	if b.baux.Occupes("iptva") != 0 {
		t.Error("la place du fournisseur n'est pas rendue")
	}
}

// US-31 CA2 : sans requete du lecteur pendant live.inactivite_transcodage, ffmpeg est arrete.
func TestInactiviteArreteFfmpeg(t *testing.T) {
	b := nouveauBancConversion(t, optionsBanc{formats: map[string]formatTest{"201": formatH264AC3},
		ajuste: func(o *Options) { o.Live.InactiviteTranscodage = config.Duree(400 * time.Millisecond) }})
	if r := b.playlist("201", "s1", ""); r.Code != 200 {
		t.Fatal(r.Code)
	}
	b.aucunResidu(t)
	// Tant que le lecteur recharge, il vit.
	if r := b.playlist("201", "s1", ""); r.Code != 200 {
		t.Fatal(r.Code)
	}
	for i := 0; i < 4; i++ {
		time.Sleep(150 * time.Millisecond)
		if r := b.playlist("201", "s1", ""); r.Code != 200 {
			t.Fatal(r.Code)
		}
	}
	if len(b.exe.appels("ffmpeg")) != 2 {
		t.Errorf("%d lancements : un lecteur actif ne doit pas perdre son ffmpeg", len(b.exe.appels("ffmpeg")))
	}
	b.aucunResidu(t)
}

// US-31 CA2 : l'arret du service tue ffmpeg et supprime le dossier ; les orphelins d'un arret
// brutal sont supprimes au demarrage (et rien d'autre).
func TestArretDuServiceEtOrphelins(t *testing.T) {
	racine := t.TempDir()
	dossier := filepath.Join(racine, "hls")
	if err := os.MkdirAll(filepath.Join(dossier, "201-0123456789ab"), 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dossier, "201-0123456789ab", "s.ts"), []byte("x"), 0o600)
	os.MkdirAll(filepath.Join(dossier, "a-garder"), 0o700)
	os.WriteFile(filepath.Join(dossier, "fichier"), []byte("x"), 0o600)
	b := nouveauBancConversion(t, optionsBanc{formats: map[string]formatTest{"201": formatH264AC3},
		ajuste: func(o *Options) { o.Live.DossierHLS = dossier }})
	b.dossier = dossier
	if e := sousDossiers(dossier); len(e) != 2 || e[0] != "a-garder" || e[1] != "fichier" {
		t.Fatalf("orphelins mal nettoyes : %v", e)
	}
	if r := b.playlist("201", "s1", ""); r.Code != 200 {
		t.Fatal(r.Code)
	}
	if processusDe(dossier) != 1 {
		t.Fatal("ffmpeg absent")
	}
	b.relais.Fermer()
	if n := processusDe(dossier); n != 0 {
		t.Errorf("%d ffmpeg apres l'arret", n)
	}
	if e := sousDossiers(dossier); len(e) != 2 {
		t.Errorf("dossier de transcodage restant : %v", e)
	}
}

// US-31 CA3 : le bail est renouvele tant que ffmpeg tourne, meme sans requete du lecteur.
func TestBailRenouveleTantQueFfmpegTourne(t *testing.T) {
	baux := &bauxFactice{}
	b := nouveauBancConversion(t, optionsBanc{formats: map[string]formatTest{"201": formatH264AC3},
		ajuste: func(o *Options) {
			o.Baux = baux
			o.Bail.TTL = config.Duree(300 * time.Millisecond)
		}})
	if r := b.playlist("201", "s1", ""); r.Code != 200 {
		t.Fatal(r.Code)
	}
	// Le fournisseur se tait : l'alimentation ne renouvelle plus rien, seul le suivi de ffmpeg le fait.
	b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleSegment, Latence: 30 * time.Second})
	b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CiblePlaylist, Latence: 30 * time.Second})
	time.Sleep(200 * time.Millisecond)
	avant := baux.bail("s1").renouvele.Load()
	time.Sleep(700 * time.Millisecond)
	if apres := baux.bail("s1").renouvele.Load(); apres-avant < 3 {
		t.Errorf("bail renouvele %d fois en 700ms avec un TTL de 300ms", apres-avant)
	}
}

// US-31 CA3 : patience 407 comme le direct, y compris au demarrage du transcodage ; ffmpeg ne
// part qu'une fois la place obtenue.
func TestTranscodagePatienteSur407(t *testing.T) {
	b := nouveauBancConversion(t, optionsBanc{formats: map[string]formatTest{"201": formatH264AC3}})
	// Verdict connu d'abord (la sonde ne doit pas absorber la panne), puis zapping : 407 repetes.
	if r := b.playlist("201", "s1", "?forcer=audio"); r.Code != 200 {
		t.Fatal(r.Code)
	}
	b.relais.FinDeLecture("s1", "201")
	b.aucunResidu(t)
	b.faux.RazCompteurs()
	b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CiblePlaylist, Fois: 3, Statut: 407})
	rec := b.playlist("201", "s1", "?forcer=audio")
	if rec.Code != 200 || rec.Header().Get("X-Mytv-Mode") != "audio" {
		t.Fatalf("HTTP %d : %s", rec.Code, rec.Body.String())
	}
	if c := b.faux.Compteurs(); c.Refus407 != 3 {
		t.Errorf("%d refus 407 essuyes", c.Refus407)
	}
	if b.relais.Etat().Patiences407 < 3 {
		t.Errorf("patiences = %d", b.relais.Etat().Patiences407)
	}
	// Patience epuisee : 503 + Retry-After, rien ne reste.
	b.relais.FinDeLecture("s1", "201")
	b.aucunResidu(t)
	b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CiblePlaylist, Statut: 407})
	rec = b.playlist("201", "s2", "?forcer=audio")
	if rec.Code != 503 || rec.Header().Get("Retry-After") == "" {
		t.Errorf("patience epuisee : HTTP %d", rec.Code)
	}
	b.aucunResidu(t)
}

// US-31 CA3 : ffmpeg qui ne demarre pas -> 502 transcodage_impossible, rien ne reste, la
// lecture suivante repart proprement.
func TestFfmpegIntrouvableOuQuiMeurtRend502(t *testing.T) {
	racine := t.TempDir()
	meurt := filepath.Join(racine, "ffmpeg-qui-meurt")
	os.WriteFile(meurt, []byte("#!/bin/sh\necho 'erreur http://u:p@h/x' >&2\nexit 1\n"), 0o700)
	muet := filepath.Join(racine, "ffmpeg-muet")
	os.WriteFile(muet, []byte("#!/bin/sh\nexec sleep 60\n"), 0o700)
	cas := map[string]string{"introuvable": "/nexiste/pas/ffmpeg", "meurt": meurt, "muet": muet}
	for nom, bin := range cas {
		t.Run(nom, func(t *testing.T) {
			b := nouveauBancConversion(t, optionsBanc{formats: map[string]formatTest{"201": formatH264AC3}, ffmpeg: bin,
				ajuste: func(o *Options) {
					o.Live.DelaiDemarrageTranscodage = config.Duree(700 * time.Millisecond)
					o.Live.DelaiSonde = config.Duree(10 * time.Second)
				}})
			debut := time.Now()
			rec := b.playlist("201", "s1", "")
			if rec.Code != 502 || b.corpsErreur(t, rec) != "transcodage_impossible" {
				t.Fatalf("HTTP %d : %s", rec.Code, rec.Body.String())
			}
			if rec.Header().Get("X-Mytv-Mode") != "audio" {
				t.Errorf("mode = %q", rec.Header().Get("X-Mytv-Mode"))
			}
			if d := time.Since(debut); d > 5*time.Second {
				t.Errorf("demarrage non borne : %v", d)
			}
			if strings.Contains(rec.Body.String(), "u:p") || strings.Contains(b.journal.String(), "u:p@") {
				t.Error("une URL de ffmpeg a fuite")
			}
			attendreQue(t, 5*time.Second, func() bool { return len(sousDossiers(b.dossier)) == 0 }, "dossier supprime")
			if n := processusDe(b.dossier); n != 0 {
				t.Errorf("%d processus restants", n)
			}
			if b.baux.Occupes("iptva") != 1 {
				t.Error("le bail de la session doit rester (la lecture peut reprendre en direct)")
			}
		})
	}
}

// US-31 CA2 : annulation. Un lecteur qui abandonne pendant le demarrage ne laisse ni ffmpeg ni dossier.
func TestAbandonPendantLeDemarrage(t *testing.T) {
	b := nouveauBancConversion(t, optionsBanc{formats: map[string]formatTest{"201": formatH264AC3}})
	b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleSegment, Latence: 5 * time.Second})
	ctx, annule := context.WithCancel(context.Background())
	req := httptest.NewRequest("GET", "/api/v1/live/201.m3u8?forcer=audio", nil).WithContext(ctx)
	req.Header.Set("X-Session", "s1")
	fini := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		b.h.ServeHTTP(rec, req)
		fini <- rec.Code
	}()
	time.Sleep(300 * time.Millisecond)
	annule()
	<-fini
	b.faux.Effacer()
	b.aucunResidu(t)
}

// US-31 CA1 : un jeton de segment local est lie a sa chaine ; Range est servi ; un faux jeton est refuse.
func TestSegmentLocalJetonLieAChaineEtRange(t *testing.T) {
	b := nouveauBancConversion(t, optionsBanc{formats: map[string]formatTest{"201": formatH264AC3, "202": formatH264AC3}})
	rec := b.playlist("201", "s1", "")
	seg := lignesSegments(rec.Body.String())[0]
	jeton := seg[strings.LastIndex(seg, "/")+1:]
	if r := b.requete("/api/v1/live/202/seg/"+jeton, "s1", nil); r.Code == 200 {
		t.Error("jeton d'une autre chaine accepte")
	}
	if r := b.requete("/api/v1/live/201/seg/"+jeton[:len(jeton)-5]+"AAAA.ts", "s1", nil); r.Code == 200 {
		t.Error("jeton falsifie accepte")
	}
	req := httptest.NewRequest("GET", seg, nil)
	req.Header.Set("X-Session", "s1")
	req.Header.Set("Range", "bytes=0-187")
	r := httptest.NewRecorder()
	b.h.ServeHTTP(r, req)
	if r.Code != 206 || r.Body.Len() != 188 || r.Body.Bytes()[0] != 0x47 {
		t.Errorf("Range : HTTP %d, %d octets", r.Code, r.Body.Len())
	}
	if r := b.requete(seg, "", nil); r.Code != 401 {
		t.Errorf("sans session : HTTP %d", r.Code)
	}
}

// US-30 CA4 / US-31 : ?forcer= prime sur la sonde et les capacites ; il ne leve pas l'interdiction
// de reencoder.
func TestForcerImposeLeModeMaisRespecteLesInterdits(t *testing.T) {
	b := nouveauBancConversion(t, optionsBanc{formats: map[string]formatTest{"203": formatH264AAC, "4k": formatHEVC4K}})
	rec := b.playlist("203", "s1", "?forcer=audio")
	if rec.Code != 200 || rec.Header().Get("X-Mytv-Mode") != "audio" {
		t.Fatalf("forcer=audio : HTTP %d mode %q", rec.Code, rec.Header().Get("X-Mytv-Mode"))
	}
	if len(b.exe.appels("ffprobe")) != 0 {
		t.Error("forcer ne doit pas sonder")
	}
	if r := b.playlist("203", "s1", "?forcer=video&cap=hevc,eac3,ac3"); r.Code != 200 || r.Header().Get("X-Mytv-Mode") != "video" {
		t.Errorf("forcer=video : HTTP %d mode %q", r.Code, r.Header().Get("X-Mytv-Mode"))
	}
	b.relais.FinDeLecture("s1", "203")
	b.aucunResidu(t)
	// 4K connue (sondee) : imposer la video est refuse (jamais un reencodage 4K).
	if r := b.playlist("4k", "s1", ""); r.Code != 415 {
		t.Fatalf("4k : HTTP %d", r.Code)
	}
	if r := b.playlist("4k", "s1", "?forcer=video"); r.Code != 415 {
		t.Errorf("forcer=video sur 4K connue : HTTP %d", r.Code)
	}
	b.aucunResidu(t)
}

func TestForcerVideoRefuseSiReencodageInterdit(t *testing.T) {
	b := nouveauBancConversion(t, optionsBanc{formats: map[string]formatTest{"203": formatH264AAC},
		ajuste: func(o *Options) { o.Live.ReencodageVideo = false }})
	if r := b.playlist("203", "s1", "?forcer=video"); r.Code != 415 {
		t.Errorf("HTTP %d", r.Code)
	}
	if len(b.exe.appels("ffmpeg")) != 0 {
		t.Error("ffmpeg lance malgre live.reencodage_video=false")
	}
}

// US-31 CA5 : mesures du delai de demarrage (requete -> premiere playlist) et du CPU de ffmpeg,
// consignees dans docs/exploitation.md. Executees seulement avec MYTV_MESURES=1 (elles ne
// verifient rien : elles chiffrent).
func TestMesuresDemarrageEtCPU(t *testing.T) {
	if os.Getenv("MYTV_MESURES") == "" {
		t.Skip("MYTV_MESURES non defini")
	}
	for _, c := range []struct {
		nom    string
		format formatTest
	}{{"audio (h264+ac3)", formatH264AC3}, {"video (hevc 640x360+eac3)", formatHEVCEAC3}} {
		b := nouveauBancConversion(t, optionsBanc{formats: map[string]formatTest{"201": c.format},
			ajuste: func(o *Options) { o.Live.SegmentHLS = 4; o.Live.ListeHLS = 6 }})
		var avant, apres syscall.Rusage
		syscall.Getrusage(syscall.RUSAGE_CHILDREN, &avant)
		debut := time.Now()
		rec := b.playlist("201", "s1", "")
		delai := time.Since(debut)
		if rec.Code != 200 {
			t.Fatalf("HTTP %d", rec.Code)
		}
		time.Sleep(5 * time.Second)
		b.relais.FinDeLecture("s1", "201")
		b.aucunResidu(t)
		syscall.Getrusage(syscall.RUSAGE_CHILDREN, &apres)
		cpu := time.Duration(apres.Utime.Nano()-avant.Utime.Nano()) + time.Duration(apres.Stime.Nano()-avant.Stime.Nano())
		t.Logf("MESURE %s : playlist en %v ; CPU enfants (ffprobe + ffmpeg) sur ~%v de vie : %v", c.nom, delai, time.Since(debut).Round(100*time.Millisecond), cpu)
	}
}
