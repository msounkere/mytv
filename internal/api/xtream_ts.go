package api

// Direct en flux MPEG-TS continu pour les box IPTV (lot 11, etape A2).
//
// MYTVOnline demande le direct en « .ts » meme reglee en HLS (essai du 22/11 : arret apres trois
// segments quand on la renvoyait vers la playlist). Une box attend alors UNE reponse sans fin. mytv
// la fabrique a partir du relais HLS existant, en interne et au nom de la session de la box : il
// lit la playlist de la chaine (meme bail, meme cache, meme recollage quand le fournisseur
// redemarre son flux), puis ecrit les segments nouveaux les uns a la suite des autres. Aucun
// ffmpeg : les segments MPEG-TS se concatenent tels quels.

import (
	"bufio"
	"bytes"
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// tsAvance : segments deja publies par lesquels la box commence. Trois : une reserve qui
	// couvre les trous du fournisseur (13 s sans segment nouveau vus le 22/11) sans partir trop
	// loin derriere le direct.
	tsAvance = 3
	// tsBourrage : intervalle des paquets nuls envoyes quand rien de nouveau n'est a ecrire.
	tsBourrage = time.Second
	// tsEchecsMax : playlists ratees de suite avant d'abandonner (la box se reconnectera).
	tsEchecsMax = 6
)

// reponseMemoire : ResponseWriter minimal qui garde le statut, les en-tetes et le corps (playlist).
type reponseMemoire struct {
	h      http.Header
	statut int
	corps  bytes.Buffer
}

func (r *reponseMemoire) Header() http.Header         { return r.h }
func (r *reponseMemoire) WriteHeader(c int)           { r.statut = c }
func (r *reponseMemoire) Write(b []byte) (int, error) { return r.corps.Write(b) }

// reponseRelais : ResponseWriter qui recopie le corps d'un segment vers la box, seulement si le
// relais repond 200 (une erreur de segment ne doit pas finir dans le flux video).
type reponseRelais struct {
	h       http.Header
	statut  int
	box     http.ResponseWriter
	ecrits  int64
	flusher http.Flusher
}

func (r *reponseRelais) Header() http.Header { return r.h }
func (r *reponseRelais) WriteHeader(c int)   { r.statut = c }
func (r *reponseRelais) Write(b []byte) (int, error) {
	if r.statut == 0 {
		r.statut = http.StatusOK
	}
	if r.statut != http.StatusOK {
		return len(b), nil
	}
	n, err := r.box.Write(b)
	r.ecrits += int64(n)
	if r.flusher != nil {
		r.flusher.Flush()
	}
	return n, err
}

// playlistTS : ce que le flux continu retient d'une playlist du relais.
type playlistTS struct {
	sequence int64
	cible    time.Duration
	segments []string // adresses relatives au relais, dans l'ordre
}

func lirePlaylistTS(corps []byte) playlistTS {
	p := playlistTS{cible: 6 * time.Second}
	sc := bufio.NewScanner(bytes.NewReader(corps))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(l, "#EXT-X-MEDIA-SEQUENCE:"):
			p.sequence, _ = strconv.ParseInt(strings.TrimPrefix(l, "#EXT-X-MEDIA-SEQUENCE:"), 10, 64)
		case strings.HasPrefix(l, "#EXT-X-TARGETDURATION:"):
			if n, err := strconv.Atoi(strings.TrimPrefix(l, "#EXT-X-TARGETDURATION:")); err == nil && n > 0 && n < 60 {
				p.cible = time.Duration(n) * time.Second
			}
		case l != "" && !strings.HasPrefix(l, "#"):
			p.segments = append(p.segments, l)
		}
	}
	return p
}

// demandeRelais : une requete GET interne au relais du direct, au nom de la session imposee.
func demandeRelais(ctx context.Context, cible string) *http.Request {
	r, _ := http.NewRequestWithContext(ctx, http.MethodGet, cible, nil)
	r.RemoteAddr = "127.0.0.1:0"
	return r
}

// xtreamLiveTS ecrit le direct de la chaine en MPEG-TS continu, jusqu'au depart de la box.
func (a *API) xtreamLiveTS(w http.ResponseWriter, req *http.Request, s sessionXtream, numero string) {
	if a.opt.Live == nil {
		http.Error(w, "direct non active", http.StatusNotImplemented)
		return
	}
	ctx := a.opt.Auth.ImposerSession(req.Context(), s.id)
	playlist := Prefixe + "/live/" + numero + ".m3u8?cap=" + xtreamCapacites
	flusher, _ := w.(http.Flusher)
	bourre := &bourreur{w: w, f: flusher}
	derniere := int64(-1) // derniere sequence ecrite
	echecs := 0
	commence := false
	debut := time.Now()
	var octets int64
	defer func() {
		if a.opt.FinLive != nil {
			// La box est partie : la place du fournisseur est rendue tout de suite.
			a.opt.FinLive.FinDeLecture(s.id, numero)
		}
		a.log.Info("xtream : fin du direct TS", "chaine", numero, "session", abreger(s.id),
			"octets", octets, "duree_s", int(time.Since(debut).Seconds()))
	}()
	for ctx.Err() == nil {
		pl := &reponseMemoire{h: http.Header{}}
		a.opt.Live.ServeHTTP(pl, demandeRelais(ctx, playlist))
		if pl.statut == 0 {
			pl.statut = http.StatusOK
		}
		if pl.statut != http.StatusOK {
			if !commence {
				// Rien n'est parti : la box recoit le vrai statut (503 occupe, 404, 415...).
				for k, v := range pl.h {
					w.Header()[k] = v
				}
				w.WriteHeader(pl.statut)
				_, _ = w.Write(pl.corps.Bytes())
				return
			}
			if echecs++; echecs >= tsEchecsMax {
				return
			}
			if bourre.attendre(ctx, 2*time.Second) != nil {
				return
			}
			continue
		}
		echecs = 0
		p := lirePlaylistTS(pl.corps.Bytes())
		if !commence {
			w.Header().Set("Content-Type", "video/mp2t")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusOK)
			commence = true
			bourre.envoye()
			a.inst.Compter("xtream_flux_total", 1, "type", "live_ts")
			// Demarrer a tsAvance segments du bord du direct.
			derniere = p.sequence + int64(len(p.segments)) - 1 - tsAvance
		}
		fin := p.sequence + int64(len(p.segments)) - 1
		if fin < derniere-int64(len(p.segments))-2 {
			// Numerotation repartie de zero (cas que le relais devrait deja recoller) : on suit.
			derniere = p.sequence - 1
		}
		nouveaux := 0
		for i, seg := range p.segments {
			seq := p.sequence + int64(i)
			if seq <= derniere {
				continue
			}
			rr := &reponseRelais{h: http.Header{}, box: w, flusher: flusher}
			a.opt.Live.ServeHTTP(rr, demandeRelais(ctx, seg))
			octets += rr.ecrits
			if ctx.Err() != nil {
				return
			}
			if rr.ecrits > 0 {
				bourre.envoye()
			}
			derniere = seq
			nouveaux++
		}
		pause := p.cible / 2
		if nouveaux > 0 {
			pause = p.cible / 3
		}
		if pause < 500*time.Millisecond {
			pause = 500 * time.Millisecond
		}
		if bourre.attendre(ctx, pause) != nil {
			return
		}
	}
}

// paquetNul : un paquet MPEG-TS de bourrage (PID 0x1FFF), que tout decodeur ignore.
var paquetNul = func() []byte {
	p := make([]byte, 188)
	p[0], p[1], p[2], p[3] = 0x47, 0x1F, 0xFF, 0x10
	for i := 4; i < len(p); i++ {
		p[i] = 0xFF
	}
	return p
}()

// bourreur garde l'instant du dernier octet envoye a la box. MYTVOnline abandonnait un flux muet
// au bout d'une dizaine de secondes, image figee puis reconnexion (22/11), pendant que le
// fournisseur tardait a publier le segment suivant : des qu'une seconde passe sans rien envoyer,
// un paquet nul part. Toujours ENTRE deux segments complets : l'alignement des paquets de 188
// octets est garde.
type bourreur struct {
	w       http.ResponseWriter
	f       http.Flusher
	dernier time.Time
}

func (b *bourreur) envoye() { b.dernier = time.Now() }

// attendre attend d en bourrant chaque fois qu'une seconde passe sans octet envoye.
func (b *bourreur) attendre(ctx context.Context, d time.Duration) error {
	fin := time.Now().Add(d)
	for {
		reste := time.Until(fin)
		if reste <= 0 {
			return nil
		}
		prochain := time.Until(b.dernier.Add(tsBourrage))
		if prochain <= 0 {
			if _, err := b.w.Write(paquetNul); err != nil {
				return err
			}
			if b.f != nil {
				b.f.Flush()
			}
			b.envoye()
			continue
		}
		if err := attendre(ctx, min(reste, prochain)); err != nil {
			return err
		}
	}
}

func attendre(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
