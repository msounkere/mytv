package hls

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
)

// Sceau chiffre et authentifie une charge courte (AES-256-GCM) avec des cles
// DERIVEES de serveur.secret_signature. C'est le mecanisme des jetons de segments ;
// il est expose pour que les autres jetons du service (images) l'utilisent tel quel
// plutot que d'en inventer un second a auditer.
//
// Un domaine (libelles de derivation) par usage : un jeton d'un domaine n'ouvre
// jamais un autre domaine, meme avec le meme secret.
type Sceau struct {
	aead     cipher.AEAD
	cleNonce []byte
}

// NouveauSceau derive les cles du domaine. Les deux libelles sont distincts pour que
// la cle de chiffrement et la cle de nonce ne se confondent jamais.
func NouveauSceau(secret []byte, libelleChiffre, libelleNonce string) (*Sceau, error) {
	if len(secret) < tailleMiniSecret {
		return nil, errors.New("hls : le secret de signature est trop court (32 octets minimum)")
	}
	bloc, err := aes.NewCipher(deriver(secret, libelleChiffre))
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(bloc)
	if err != nil {
		return nil, err
	}
	return &Sceau{aead: aead, cleNonce: deriver(secret, libelleNonce)}, nil
}

// Sceller produit « <nonce || charge chiffree || etiquette> » en base64url, LIE a
// `portee` : un jeton scelle pour une portee n'ouvre pas une autre portee.
//
// Le nonce est DERIVE de la charge (HMAC, chiffrement de type SIV) : une meme charge
// donne toujours le meme jeton, ce qui garde stables les URL emises. Il ne revele que
// l'egalite de deux charges.
func (s *Sceau) Sceller(portee, charge string) string {
	charge = portee + "\x00" + charge
	m := hmac.New(sha256.New, s.cleNonce)
	m.Write([]byte(charge))
	nonce := m.Sum(nil)[:s.aead.NonceSize()]
	scelle := s.aead.Seal(append([]byte(nil), nonce...), nonce, []byte(charge), nil)
	return base64.RawURLEncoding.EncodeToString(scelle)
}

// Ouvrir rend la charge si, et seulement si, le jeton est authentique pour `portee`.
// Une extension (« .ts », « .m3u8 ») ajoutee apres le jeton est ignoree.
func (s *Sceau) Ouvrir(portee, jeton string) (string, error) {
	morceaux := strings.Split(jeton, ".")
	if len(morceaux) > 2 {
		return "", ErrJeton
	}
	brut, err := base64.RawURLEncoding.DecodeString(morceaux[0])
	if err != nil || len(brut) < s.aead.NonceSize()+s.aead.Overhead() {
		return "", ErrJeton
	}
	n := s.aead.NonceSize()
	clair, err := s.aead.Open(nil, brut[:n], brut[n:], nil)
	if err != nil {
		return "", ErrJeton
	}
	charge, ok := strings.CutPrefix(string(clair), portee+"\x00")
	if !ok {
		return "", ErrJeton
	}
	return charge, nil
}
