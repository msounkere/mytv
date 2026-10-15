package remux

import (
	"errors"
	"fmt"
	"net/url"
)

// ErrArgumentRefuse : une valeur destinee a ffmpeg ou ffprobe n'a pas passe la validation.
var ErrArgumentRefuse = errors.New("remux : argument refuse")

const (
	urlMax     = 2048
	agentMax   = 300
	protocoles = "http,https,tcp,tls,crypto"
	// protocolesEntree : ffmpeg ne lit que son entree standard.
	protocolesEntree = "pipe"
	// protocolesSonde : ffprobe ne parle qu'a l'adresse locale de fluxSonde.
	protocolesSonde = "http,tcp"
	enteteAgent     = "User-Agent"
	decalageMaxHs   = 72 // un seek au-dela de 72 h n'a aucun sens pour un film
)

// validerURL accepte uniquement une adresse http(s) absolue, sans caractere de
// controle ni espace. Le schema impose commence par une lettre : l'argument ne
// peut donc jamais ressembler a une option (« -i », « -f »...). Combine a
// -protocol_whitelist, une redirection ou une playlist ne peut pas faire lire un
// fichier local a ffmpeg.
func validerURL(brut string) error {
	if brut == "" || len(brut) > urlMax {
		return fmt.Errorf("%w : adresse vide ou trop longue", ErrArgumentRefuse)
	}
	for _, r := range brut {
		if r <= ' ' || r == 0x7f {
			return fmt.Errorf("%w : adresse avec caractere de controle", ErrArgumentRefuse)
		}
	}
	u, err := url.Parse(brut)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("%w : adresse http(s) absolue attendue", ErrArgumentRefuse)
	}
	return nil
}
