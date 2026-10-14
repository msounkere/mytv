package comptes

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Parametres d'argon2id. Ils sont ECRITS dans chaque empreinte : changer la config
// n'invalide aucun mot de passe existant, et l'empreinte est recalculee a la
// prochaine connexion reussie (voir Service.Connecter).
type parametres struct {
	memoireKio   uint32
	iterations   uint32
	parallelisme uint8
}

const (
	tailleSel   = 16
	tailleCle   = 32
	longueurMax = 1024 // octets : un mot de passe geant ne doit pas devenir un deni de service
)

var errEmpreinte = errors.New("empreinte de mot de passe illisible")

// hacher produit « $argon2id$v=19$m=..,t=..,p=..$sel$cle » (format PHC).
func hacher(motDePasse string, p parametres) (string, error) {
	sel := make([]byte, tailleSel)
	if _, err := rand.Read(sel); err != nil {
		return "", fmt.Errorf("tirage du sel : %w", err)
	}
	cle := argon2.IDKey([]byte(motDePasse), sel, p.iterations, p.memoireKio, p.parallelisme, tailleCle)
	enc := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version,
		p.memoireKio, p.iterations, p.parallelisme, enc.EncodeToString(sel), enc.EncodeToString(cle)), nil
}

func lireEmpreinte(h string) (p parametres, sel, cle []byte, err error) {
	parts := strings.Split(h, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return p, nil, nil, errEmpreinte
	}
	var version int
	if _, e := fmt.Sscanf(parts[2], "v=%d", &version); e != nil || version != argon2.Version {
		return p, nil, nil, errEmpreinte
	}
	var m, t, par uint32
	if _, e := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &par); e != nil || par == 0 || par > 255 || t == 0 || m == 0 {
		return p, nil, nil, errEmpreinte
	}
	enc := base64.RawStdEncoding
	if sel, err = enc.DecodeString(parts[4]); err != nil || len(sel) == 0 {
		return p, nil, nil, errEmpreinte
	}
	if cle, err = enc.DecodeString(parts[5]); err != nil || len(cle) == 0 {
		return p, nil, nil, errEmpreinte
	}
	return parametres{m, t, uint8(par)}, sel, cle, nil
}

// verifier compare en temps constant. Il renvoie aussi les parametres de
// l'empreinte, pour que l'appelant sache si elle est a renouveler.
func verifier(motDePasse, empreinte string) (ok bool, p parametres) {
	p, sel, cle, err := lireEmpreinte(empreinte)
	if err != nil {
		return false, p
	}
	calc := argon2.IDKey([]byte(motDePasse), sel, p.iterations, p.memoireKio, p.parallelisme, uint32(len(cle)))
	return subtle.ConstantTimeCompare(calc, cle) == 1, p
}
