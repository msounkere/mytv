// Package epg ingere les guides XMLTV des fournisseurs, rapproche leurs chaines du
// catalogue et les stocke dans SQLite (US-09).
package epg

import (
	"regexp"
	"strings"
	"unicode"
)

// pays : prefixes que les fournisseurs collent aux noms de chaines (« FR: TF1 »,
// « |FR| TF1 », « [BE] RTL »). Une liste fermee : « AB1 » ou « TV5 » ne doivent
// pas perdre leur debut. Le prefixe n'est retire que suivi d'un separateur.
const codesPays = "fr|be|ch|ca|us|uk|gb|de|es|it|pt|nl|lu|ar|ma|dz|tn|tr|ru|pl|ro|se|no|dk|fi|gr|ie|br|mx|in|pk|al|vip"

var (
	rePrefixePays = regexp.MustCompile(`^[\s|\[(]*(?:` + codesPays + `)\s*(?:[:|/\-]|\]|\))[\s|:/\-\])]*`)
	// Qualite / marqueurs techniques, comme MOTS ENTIERS (« HD » dans « BEIN SPORTS HD »
	// mais pas dans « HDTV5 »).
	motsQualite = map[string]bool{
		"hd": true, "fhd": true, "uhd": true, "sd": true, "4k": true, "8k": true, "hevc": true,
		"h265": true, "h264": true, "raw": true, "50fps": true, "60fps": true, "fullhd": true,
		"hq": true, "lq": true, "multisub": true,
	}
)

// Normaliser donne la cle de rapprochement par nom : minuscules, sans accents,
// sans prefixe pays ni marqueurs de qualite, ponctuation ramenee a une espace.
// « |FR| Éclair TV HD » et « eclair tv » donnent la meme cle.
func Normaliser(nom string) string {
	s := strings.ToLower(nom)
	s = rePrefixePays.ReplaceAllString(s, "")
	s = sansAccents(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		case r == '+': // « TF1 +1 » (decale) differe de « TF1 »
			b.WriteString(" plus ")
		default:
			b.WriteByte(' ')
		}
	}
	mots := strings.Fields(b.String())
	garde := mots[:0]
	for _, m := range mots {
		if !motsQualite[m] {
			garde = append(garde, m)
		}
	}
	return strings.Join(garde, " ")
}

// sansAccents replie les lettres latines accentuees. Table minimale a la main : le
// projet n'a pas golang.org/x/text et l'ajouter pour cela serait disproportionne.
func sansAccents(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0xC0 {
			return r
		}
		if rr, ok := replis[r]; ok {
			return rr
		}
		return r
	}, s)
}

var replis = func() map[rune]rune {
	m := map[rune]rune{}
	for base, lettres := range map[rune]string{
		'a': "àáâãäåāăą", 'c': "çćĉċč", 'd': "ďđ", 'e': "èéêëēĕėęě", 'g': "ĝğġģ", 'h': "ĥħ",
		'i': "ìíîïĩīĭįı", 'j': "ĵ", 'k': "ķ", 'l': "ĺļľŀł", 'n': "ñńņňŉ", 'o': "òóôõöøōŏő",
		'r': "ŕŗř", 's': "śŝşš", 't': "ţťŧ", 'u': "ùúûüũūŭůűų", 'w': "ŵ", 'y': "ýÿŷ", 'z': "źżž",
	} {
		for _, r := range lettres {
			m[r] = base
		}
	}
	m['æ'], m['œ'], m['ß'] = 'e', 'e', 's' // approximation volontaire (une seule rune)
	return m
}()
