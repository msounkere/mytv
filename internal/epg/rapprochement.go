package epg

import (
	"sort"
	"strings"

	"mytv/internal/domain"
)

// Rapprocher lie chaque chaine du catalogue a un canal du guide.
//
//  1. par epg_id : l'identifiant donne par le fournisseur fait foi s'il existe dans
//     le guide (exactement, sinon a la casse pres quand c'est sans ambiguite) ;
//  2. a defaut, par nom normalise (voir Normaliser) contre les noms d'affichage des
//     canaux. Si PLUSIEURS canaux distincts portent ce nom, la chaine est
//     marquee Ambigu et ne recoit aucun epg_id : deviner afficherait le
//     programme d'une autre chaine, pire que pas de programme.
//
// Les chaines sans correspondance ne produisent AUCUNE ligne (introuvable =
// absent). Le resultat suit l'ordre des chaines.
func Rapprocher(chaines []domain.Chaine, canaux []Canal) []domain.Rapprochement {
	ids := make(map[string]bool, len(canaux))
	parCasse := map[string][]string{}
	parNom := map[string][]string{} // nom normalise -> ids de canaux distincts, tries
	for _, c := range canaux {
		ids[c.ID] = true
	}
	for id := range ids {
		k := strings.ToLower(id)
		parCasse[k] = append(parCasse[k], id)
	}
	for _, c := range canaux {
		vus := map[string]bool{}
		for _, n := range c.Noms {
			k := Normaliser(n)
			if k == "" || vus[k] {
				continue
			}
			vus[k] = true
			parNom[k] = append(parNom[k], c.ID)
		}
	}
	for k, l := range parNom {
		parNom[k] = dedoublonner(l)
	}

	var res []domain.Rapprochement
	for _, ch := range chaines {
		if id := ch.EpgID; id != "" {
			if ids[id] {
				res = append(res, domain.Rapprochement{Chaine: ch.ID, EpgID: id, Methode: domain.RapprocheParEpgID})
				continue
			}
			if l := parCasse[strings.ToLower(id)]; len(l) == 1 {
				res = append(res, domain.Rapprochement{Chaine: ch.ID, EpgID: l[0], Methode: domain.RapprocheParEpgID})
				continue
			}
		}
		k := Normaliser(ch.Nom)
		if k == "" {
			continue
		}
		switch l := parNom[k]; len(l) {
		case 0:
		case 1:
			res = append(res, domain.Rapprochement{Chaine: ch.ID, EpgID: l[0], Methode: domain.RapprocheParNom})
		default:
			res = append(res, domain.Rapprochement{Chaine: ch.ID, Methode: domain.RapprocheParNom, Ambigu: true})
		}
	}
	return res
}

func dedoublonner(l []string) []string {
	sort.Strings(l)
	out := l[:0]
	for i, s := range l {
		if i == 0 || s != l[i-1] {
			out = append(out, s)
		}
	}
	return out
}
