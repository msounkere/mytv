package remux

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// ErrArgumentRefuse : une valeur destinee a ffmpeg ou ffprobe n'a pas passe la validation.
var ErrArgumentRefuse = errors.New("remux : argument refuse")

const (
	urlMax        = 2048
	agentMax      = 300
	protocoles    = "http,https,tcp,tls,crypto"
	enteteAgent   = "User-Agent"
	decalageMaxHs = 72 // un seek au-dela de 72 h n'a aucun sens pour un film
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

// agentDepuis extrait le seul en-tete que l'on transmet a ffmpeg (-user_agent).
// Tout autre en-tete est refuse : ffmpeg les prendrait via -headers, une chaine
// brute ou un saut de ligne ouvrirait l'injection d'en-tetes.
func agentDepuis(h http.Header) (string, error) {
	agent := ""
	for k, v := range h {
		if !strings.EqualFold(k, enteteAgent) {
			return "", fmt.Errorf("%w : en-tete %q non transmissible", ErrArgumentRefuse, k)
		}
		if len(v) > 0 {
			agent = v[0]
		}
	}
	if len(agent) > agentMax {
		return "", fmt.Errorf("%w : User-Agent trop long", ErrArgumentRefuse)
	}
	for _, r := range agent {
		if r < ' ' || r == 0x7f {
			return "", fmt.Errorf("%w : User-Agent avec caractere de controle", ErrArgumentRefuse)
		}
	}
	return agent, nil
}
