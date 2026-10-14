package xtream

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// Le fournisseur melange librement les types : « 12 » ou 12, null, champ absent,
// parfois un booleen a la place d'une chaine vide. Chaque type ci-dessous accepte
// toutes ces formes et se rabat sur la valeur zero plutot que de faire echouer
// tout un catalogue de 60 000 lignes a cause d'un champ douteux.

// texte accepte chaine, nombre, booleen ou null.
type texte string

func (t *texte) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	switch {
	case len(b) == 0, string(b) == "null", string(b) == "false", string(b) == "true":
		*t = ""
	case b[0] == '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*t = texte(strings.TrimSpace(s))
	case b[0] == '{' || b[0] == '[':
		*t = "" // structure inattendue : ignoree
	default:
		// Un nombre est garde tel qu'ecrit : « 845410 » ne doit jamais devenir « 8.4541e+05 ».
		var n json.Number
		if err := json.Unmarshal(b, &n); err != nil {
			return err
		}
		*t = texte(normaliserNombre(n.String()))
	}
	return nil
}

func (t texte) String() string { return string(t) }

// entier accepte 12, "12", "12.0", "" et null (0 si illisible).
type entier int

func (e *entier) UnmarshalJSON(b []byte) error {
	var t texte
	if err := t.UnmarshalJSON(b); err != nil {
		return err
	}
	*e = entier(analyserEntier(string(t)))
	return nil
}

func analyserEntier(s string) int64 {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return int64(f)
	}
	return 0
}

// decimal accepte 7.5, "7.5", "" et null.
type decimal float64

func (d *decimal) UnmarshalJSON(b []byte) error {
	var t texte
	if err := t.UnmarshalJSON(b); err != nil {
		return err
	}
	f, err := strconv.ParseFloat(string(t), 64)
	if err != nil {
		f = 0
	}
	*d = decimal(f)
	return nil
}

// instant : « added » est un nombre de secondes depuis 1970 ecrit en chaine.
type instant string

func (i *instant) UnmarshalJSON(b []byte) error {
	var t texte
	if err := t.UnmarshalJSON(b); err != nil {
		return err
	}
	*i = instant(t)
	return nil
}

func (i instant) Heure() time.Time {
	n := analyserEntier(string(i))
	if n <= 0 {
		return time.Time{}
	}
	return time.Unix(n, 0).UTC()
}

// annee extrait l'annee d'une date « 2020-05-01 », ou renvoie la valeur telle quelle.
func annee(valeurs ...texte) string {
	for _, v := range valeurs {
		if len(v) >= 4 {
			if _, err := strconv.Atoi(string(v[:4])); err == nil {
				return string(v[:4])
			}
		}
	}
	return ""
}

type categorieJSON struct {
	ID  texte `json:"category_id"`
	Nom texte `json:"category_name"`
}

type chaineJSON struct {
	Num         entier `json:"num"`
	Nom         texte  `json:"name"`
	StreamID    texte  `json:"stream_id"`
	Icone       texte  `json:"stream_icon"`
	EpgID       texte  `json:"epg_channel_id"`
	CategorieID texte  `json:"category_id"`
}

type filmJSON struct {
	Nom         texte   `json:"name"`
	StreamID    texte   `json:"stream_id"`
	Icone       texte   `json:"stream_icon"`
	CategorieID texte   `json:"category_id"`
	Extension   texte   `json:"container_extension"`
	Note        decimal `json:"rating"`
	Ajoute      instant `json:"added"`
	Annee       texte   `json:"year"`
	Sortie      texte   `json:"releaseDate"`
	SortieAlt   texte   `json:"release_date"`
}

type serieJSON struct {
	Nom         texte   `json:"name"`
	SerieID     texte   `json:"series_id"`
	Couverture  texte   `json:"cover"`
	CategorieID texte   `json:"category_id"`
	Note        decimal `json:"rating"`
	Annee       texte   `json:"year"`
	Sortie      texte   `json:"releaseDate"`
	SortieAlt   texte   `json:"release_date"`
}

// normaliserNombre ramene « 845412.0 » a « 845412 » : un identifiant ecrit en
// flottant par le fournisseur doit rester le meme identifiant.
func normaliserNombre(s string) string {
	if !strings.ContainsAny(s, ".eE") {
		return s
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil && f == float64(int64(f)) {
		return strconv.FormatInt(int64(f), 10)
	}
	return s
}
