// Package hls reecrit et signe les playlists HLS du fournisseur.
//
// Inspire de TuliProx (https://github.com/euzu/tuliprox, licence MIT,
// Copyright (c) 2021 euzu), fichier backend/src/processing/parser/hls.rs : la
// playlist du fournisseur est telechargee puis CHAQUE URL est remplacee par une
// URL de notre service. Differences voulues :
//   - les URL reecrites sont RELATIVES a la racine (« /api/v1/... ») : elles
//     marchent depuis le domaine public, le reseau local ou un Chromecast, sans
//     connaitre l'adresse par laquelle on nous joint ;
//   - le jeton est CHIFFRE et authentifie (AES-256-GCM) : le client n'y lit ni
//     l'hote du fournisseur ni les identifiants que porte souvent l'URL, et il ne
//     peut servir qu'a lire une URL que NOUS avons emise, ce qui interdit
//     d'utiliser le service comme relais ouvert vers n'importe quel hote ;
//   - les URL de segments sont STABLES d'une playlist a l'autre (voir table.go).
//
// Pour les appelants (internal/live) : un lecteur annule des requetes de segment
// en vol pour ses propres raisons (zapping, saut, purge de tampon, delai) ; le bail
// (un seul par fournisseur) ne doit JAMAIS etre libere sur une telle annulation,
// seulement par la fin de lecture explicite ou son expiration.
package hls

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
)

const (
	// tailleMiniSecret : un secret court se devine ; la configuration l'exige aussi.
	tailleMiniSecret = 32
	// prefixeCle distingue un jeton « nom de segment » d'un jeton « URL complete ».
	prefixeCle = "seg:"
	// Libelles de derivation : une cle par usage, jamais le secret brut.
	libelleChiffrement = "mytv/hls/jeton/chiffrement/v1"
	libelleNonce       = "mytv/hls/jeton/nonce/v1"
)

var (
	ErrJeton   = errors.New("hls : jeton invalide")
	ErrInconnu = errors.New("hls : segment inconnu (playlist a recharger)")
)

var reURI = regexp.MustCompile(`URI="([^"]+)"`)

// Options regle la table d'adresses ; les deux valeurs sont OBLIGATOIRES (elles
// viennent de config.Live.TableSegments : le paquet n'a pas de reglage cache).
type Options struct {
	Capacite int
	Validite time.Duration
	Horloge  func() time.Time // injectable pour les tests ; time.Now si absent
}

// Reecrivain reecrit les playlists et verifie les jetons qu'il a emis.
type Reecrivain struct {
	sceau *Sceau
	table *table
}

// Cible est ce qu'un jeton valide donne le droit de lire.
type Cible struct {
	URL string // adresse reelle chez le fournisseur
	// Cle identifie la ressource de facon stable (nom de segment, sans hash de
	// chemin) : c'est la bonne cle de cache, l'URL reelle changeant a chaque playlist.
	Cle string
}

func Nouveau(secret []byte, opt Options) (*Reecrivain, error) {
	if len(secret) < tailleMiniSecret {
		return nil, errors.New("hls : le secret de signature est trop court (32 octets minimum)")
	}
	if opt.Capacite <= 0 || opt.Validite <= 0 {
		return nil, errors.New("hls : capacite et validite de la table d'adresses doivent etre > 0 (live.table_segments)")
	}
	if opt.Horloge == nil {
		opt.Horloge = time.Now
	}
	sceau, err := NouveauSceau(secret, libelleChiffrement, libelleNonce)
	if err != nil {
		return nil, err
	}
	return &Reecrivain{sceau: sceau, table: nouvelleTable(opt.Capacite, opt.Validite, opt.Horloge)}, nil
}

// deriver tire une sous-cle de 32 octets du secret, une par usage (HMAC-SHA256).
func deriver(secret []byte, libelle string) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(libelle))
	return m.Sum(nil)
}

// signer scelle `charge` pour la chaine (voir Sceau).
//
// POURQUOI chiffrer et pas seulement signer : une simple signature laisse la charge
// LISIBLE par le client (base64), donc l'hote du fournisseur et les identifiants
// que porte l'URL (/live/<u>/<p>/..., /movie/<u>/<p>/..., URI de cle).
//
// Le jeton est LIE a la chaine : un jeton emis pour une chaine ne sert pas sur une
// autre (sinon un client pourrait faire relayer a une chaine qu'il n'a pas ouverte
// le contenu d'une autre, et contourner le bail pris pour la sienne).
func (r *Reecrivain) signer(chaine, charge string) string { return r.sceau.Sceller(chaine, charge) }

// verifier rend la charge si, et seulement si, le jeton est authentique.
func (r *Reecrivain) verifier(chaine, jeton string) (string, error) {
	return r.sceau.Ouvrir(chaine, jeton)
}

// Resoudre rend ce que le jeton autorise, ou une erreur. C'est le SEUL chemin
// pour obtenir une adresse a relayer : refus par defaut.
//
// `chaine` est celle du CHEMIN de la requete : un jeton emis pour une autre chaine est refuse.
func (r *Reecrivain) Resoudre(chaine, jeton string) (Cible, error) {
	charge, err := r.verifier(chaine, jeton)
	if err != nil {
		return Cible{}, err
	}
	if cle, ok := strings.CutPrefix(charge, prefixeCle); ok {
		u, trouve := r.table.chercher(cle)
		if !trouve {
			return Cible{}, ErrInconnu
		}
		return Cible{URL: u, Cle: prefixeCle + cle}, nil
	}
	if !estHTTP(charge) {
		return Cible{}, ErrJeton
	}
	return Cible{URL: charge, Cle: charge}, nil
}

func estHTTP(u string) bool {
	p, err := url.Parse(u)
	return err == nil && (p.Scheme == "http" || p.Scheme == "https") && p.Host != ""
}

// resoudre rend l'URL absolue d'une reference, relativement a la playlist.
func resoudre(base *url.URL, ref string) string {
	r, err := url.Parse(strings.TrimSpace(ref))
	if err != nil {
		return ref
	}
	return base.ResolveReference(r).String()
}

// Reecrire remplace chaque URL de la playlist par une URL signee de ce service.
//
// `prefixe` est la racine des routes de segments, par exemple
// « /api/v1/live/845410/seg » ; `portee` isole les noms de segments d'une chaine
// (ou d'une rendition) de ceux d'une autre ; `base` est l'adresse de la playlist
// APRES redirections, qui sert a resoudre les URL relatives.
//
// Les lignes de segment portent un jeton qui ne depend que de portee + nom de
// fichier (« 845410_153.ts ») : POURQUOI, le fournisseur glisse un hash different
// dans le chemin a chaque playlist, et hls.js declare « media sequence mismatch »
// (erreur fatale) si l'URL d'un meme numero de segment differe d'une playlist a la
// suivante. L'adresse reelle la plus recente est gardee dans la table.
//
// `portee` commence par l'identifiant de la chaine (« 845410 » ou « 845410/index.m3u8 »),
// auquel tous les jetons emis sont lies.
func (r *Reecrivain) Reecrire(prefixe, portee string, base *url.URL, contenu string) string {
	chaine, _, _ := strings.Cut(portee, "/")
	var sortie strings.Builder
	for _, ligne := range strings.Split(strings.ReplaceAll(contenu, "\r\n", "\n"), "\n") {
		ligne = strings.TrimRight(ligne, "\r")
		if strings.TrimSpace(ligne) == "" {
			continue
		}
		if strings.HasPrefix(ligne, "#") {
			// Attributs URI="..." : cles de chiffrement, segment d'initialisation.
			ligne = reURI.ReplaceAllStringFunc(ligne, func(m string) string {
				cible := resoudre(base, reURI.FindStringSubmatch(m)[1])
				if !estHTTP(cible) {
					return m // « data: » etc. : rien a relayer
				}
				// Une sous-playlist (EXT-X-MEDIA audio, sous-titres) doit etre reconnue
				// comme telle pour etre reecrite a son tour. Les autres (cle de
				// chiffrement, segment d'init) gardent l'URL complete et NON la table :
				// une cle qui tourne sous un meme nom de fichier ne doit pas etre remplacee.
				jeton := r.signer(chaine, cible)
				if estPlaylist(cible) {
					jeton += ".m3u8"
				}
				return `URI="` + prefixe + "/" + jeton + `"`
			})
			sortie.WriteString(ligne + "\n")
			continue
		}
		cible := resoudre(base, ligne)
		if !estHTTP(cible) {
			continue // jamais de jeton pour autre chose que http(s)
		}
		sortie.WriteString(prefixe + "/" + r.jetonLigne(chaine, portee, cible) + "\n")
	}
	return sortie.String()
}

func (r *Reecrivain) jetonLigne(chaine, portee, cible string) string {
	u, _ := url.Parse(cible)
	nom := path.Base(u.Path)
	if estPlaylist(cible) {
		// Sous-playlist : reecrite a son tour quand on la lira (CA4).
		return r.signer(chaine, cible) + ".m3u8"
	}
	if nom == "." || nom == "/" || strings.ContainsAny(nom, ":") {
		return r.signer(chaine, cible) + ".ts"
	}
	cle := portee + "/" + nom
	r.table.noter(cle, cible)
	return r.signer(chaine, prefixeCle+cle) + extension(nom)
}

func estPlaylist(cible string) bool {
	u, err := url.Parse(cible)
	return err == nil && strings.HasSuffix(strings.ToLower(u.Path), ".m3u8")
}

// extension garde celle du segment (.ts, .m4s, .aac...) si elle est plausible.
func extension(nom string) string {
	e := path.Ext(nom)
	if len(e) < 2 || len(e) > 6 || strings.Trim(e[1:], "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789") != "" {
		return ".ts"
	}
	return e
}
