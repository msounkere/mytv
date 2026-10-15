package telemetrie

import (
	"regexp"
	"strings"
)

// Les URL de lecture portent des identifiants dans le chemin
// (/live/<u>/<p>/12.m3u8) et des jetons signes ; l'interface envoie parfois des
// adresses telles qu'elle les connait. Tout texte est masque AVANT d'etre ecrit.
var (
	reIdentifiantsChemin = regexp.MustCompile(`(/(?:live|movie|series|hls|resource)/)[^/\s"']+/[^/\s"']+`)
	reJetonLong          = regexp.MustCompile(`/[A-Za-z0-9_\-.=+]{30,}`)
	// Une chaine de requete peut contenir mot de passe, jeton, signature : elle est supprimee.
	reRequete = regexp.MustCompile(`\?[^\s"'#]*`)
	reAuth    = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]+`)
	reSecret  = regexp.MustCompile(`(?i)\b(password|passwd|mot_de_passe|token|access_token|refresh_token|jeton|secret|signature|sig|cookie|session|sessionid|sid|auth|authorization|apikey|api_key)\s*[=:]\s*[^\s,;"']+`)
	reHote    = regexp.MustCompile(`(?i)(https?://)[^/\s"']*@`)
)

// Masquer retire d'un texte ce qui pourrait identifier ou authentifier.
func Masquer(s string) string {
	s = reHote.ReplaceAllString(s, "${1}***@")
	s = reAuth.ReplaceAllString(s, "$1 ***")
	s = reSecret.ReplaceAllString(s, "$1=***")
	s = reRequete.ReplaceAllString(s, "?…")
	s = reIdentifiantsChemin.ReplaceAllString(s, "${1}***/***")
	s = reJetonLong.ReplaceAllString(s, "/…")
	return s
}

// tronquer coupe a n octets sans casser un caractere UTF-8.
func tronquer(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !validFin(s) {
		s = s[:len(s)-1]
	}
	return s
}

func validFin(s string) bool {
	return strings.ToValidUTF8(s, "") == s
}

// nettoyer reduit une valeur JSON a des types simples, masque les chaines et borne la taille.
func nettoyer(v any, texteMax, profondeur int) any {
	if profondeur > 3 {
		return nil
	}
	switch x := v.(type) {
	case string:
		return tronquer(Masquer(x), texteMax)
	case float64, bool, nil:
		return x
	case []any:
		out := make([]any, 0, min(len(x), 20))
		for i, e := range x {
			if i >= 20 {
				break
			}
			out = append(out, nettoyer(e, texteMax, profondeur+1))
		}
		return out
	case map[string]any:
		out := map[string]any{}
		n := 0
		for k, e := range x {
			if n >= 30 {
				break
			}
			if cleSensible(k) {
				out[tronquer(Masquer(k), 40)] = "***"
				n++
				continue
			}
			out[tronquer(Masquer(k), 40)] = nettoyer(e, texteMax, profondeur+1)
			n++
		}
		return out
	}
	return nil
}

var reCleSensible = regexp.MustCompile(`(?i)pass|token|jeton|secret|cookie|auth|session|signature|^sig$|apikey|api_key|credential`)

// cleSensible : le NOM du champ suffit a savoir que sa valeur ne doit pas etre gardee.
func cleSensible(k string) bool { return reCleSensible.MatchString(k) }
