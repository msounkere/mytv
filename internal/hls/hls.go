// Package hls reecrit et signe les playlists HLS du fournisseur.
//
// Inspire de TuliProx (https://github.com/euzu/tuliprox, licence MIT,
// Copyright (c) 2021 euzu), fichier backend/src/processing/parser/hls.rs : la
// playlist du fournisseur est telechargee puis CHAQUE URL est remplacee par une
// URL de notre service. Differences voulues :
//   - les URL reecrites sont RELATIVES a la racine (« /api/v1/... ») : elles
//     marchent depuis le domaine public, le reseau local ou un Chromecast, sans
//     connaitre l'adresse par laquelle on nous joint ;
//   - le jeton est signe par HMAC et non obscurci : il ne peut servir qu'a lire
//     une URL que NOUS avons emise, ce qui interdit d'utiliser le service comme
//     relais ouvert vers n'importe quel hote ;
//   - les URL de segments sont STABLES d'une playlist a l'autre (voir table.go).
package hls

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
)

const (
	// CapaciteParDefaut : segments retenus dans la table d'adresses. Une chaine
	// annonce une dizaine de segments a la fois ; cela couvre de nombreuses
	// chaines zappees sans que la memoire ne derive.
	CapaciteParDefaut = 600
	// ValiditeParDefaut : au-dela, un segment n'est plus annonce par personne.
	ValiditeParDefaut = 10 * time.Minute

	// tailleMiniSecret : un secret court se devine ; configuration l'exige aussi.
	tailleMiniSecret = 32
	// octetsSignature : 128 bits de HMAC-SHA256 suffisent contre la falsification
	// et gardent les URL courtes.
	octetsSignature = 16
	// prefixeCle distingue un jeton « nom de segment » d'un jeton « URL complete ».
	prefixeCle = "seg:"
)

var (
	ErrJeton   = errors.New("hls : jeton invalide")
	ErrInconnu = errors.New("hls : segment inconnu (playlist a recharger)")
)

var reURI = regexp.MustCompile(`URI="([^"]+)"`)

// Options regle la table d'adresses ; les zeros prennent les valeurs par defaut.
type Options struct {
	Capacite int
	Validite time.Duration
	Horloge  func() time.Time // injectable pour les tests
}

// Reecrivain reecrit les playlists et verifie les jetons qu'il a emis.
type Reecrivain struct {
	secret []byte
	table  *table
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
	if opt.Capacite <= 0 {
		opt.Capacite = CapaciteParDefaut
	}
	if opt.Validite <= 0 {
		opt.Validite = ValiditeParDefaut
	}
	if opt.Horloge == nil {
		opt.Horloge = time.Now
	}
	return &Reecrivain{
		secret: append([]byte(nil), secret...),
		table:  nouvelleTable(opt.Capacite, opt.Validite, opt.Horloge),
	}, nil
}

// signer produit « <charge en base64url>.<signature> ».
func (r *Reecrivain) signer(charge string) string {
	c := base64.RawURLEncoding.EncodeToString([]byte(charge))
	return c + "." + r.signature(c)
}

func (r *Reecrivain) signature(charge string) string {
	m := hmac.New(sha256.New, r.secret)
	m.Write([]byte(charge))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:octetsSignature])
}

// verifier rend la charge si, et seulement si, la signature est valide. Le jeton
// peut porter une extension (« .ts ») ajoutee pour les lecteurs qui devinent le
// type d'apres l'URL ; elle n'est pas signee et n'a aucun effet.
func (r *Reecrivain) verifier(jeton string) (string, error) {
	morceaux := strings.Split(jeton, ".")
	if len(morceaux) < 2 || len(morceaux) > 3 {
		return "", ErrJeton
	}
	charge, sig := morceaux[0], morceaux[1]
	if !hmac.Equal([]byte(sig), []byte(r.signature(charge))) {
		return "", ErrJeton
	}
	brut, err := base64.RawURLEncoding.DecodeString(charge)
	if err != nil {
		return "", ErrJeton
	}
	return string(brut), nil
}

// Resoudre rend ce que le jeton autorise, ou une erreur. C'est le SEUL chemin
// pour obtenir une adresse a relayer : refus par defaut.
func (r *Reecrivain) Resoudre(jeton string) (Cible, error) {
	charge, err := r.verifier(jeton)
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
func (r *Reecrivain) Reecrire(prefixe, portee string, base *url.URL, contenu string) string {
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
				return `URI="` + prefixe + "/" + r.signer(cible) + `"`
			})
			sortie.WriteString(ligne + "\n")
			continue
		}
		cible := resoudre(base, ligne)
		if !estHTTP(cible) {
			continue // jamais de jeton pour autre chose que http(s)
		}
		sortie.WriteString(prefixe + "/" + r.jetonLigne(portee, cible) + "\n")
	}
	return sortie.String()
}

func (r *Reecrivain) jetonLigne(portee, cible string) string {
	u, _ := url.Parse(cible)
	nom := path.Base(u.Path)
	if strings.HasSuffix(strings.ToLower(u.Path), ".m3u8") {
		// Sous-playlist : reecrite a son tour quand on la lira (CA4).
		return r.signer(cible) + ".m3u8"
	}
	if nom == "." || nom == "/" || strings.ContainsAny(nom, ":") {
		return r.signer(cible) + ".ts"
	}
	cle := portee + "/" + nom
	r.table.noter(cle, cible)
	return r.signer(prefixeCle+cle) + extension(nom)
}

// extension garde celle du segment (.ts, .m4s, .aac...) si elle est plausible.
func extension(nom string) string {
	e := path.Ext(nom)
	if len(e) < 2 || len(e) > 6 || strings.Trim(e[1:], "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789") != "" {
		return ".ts"
	}
	return e
}
