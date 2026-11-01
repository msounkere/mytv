package preparation

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"mytv/internal/domain"
)

// voulu est un fichier a tenir pret, dans l'ordre de priorite du plan.
type voulu struct {
	contenu   domain.ContenuID
	fid       string
	numero    string
	extension string
	episode   bool
	serie     domain.ContenuID
	saison    int
	numEp     int
}

// planProfil est ce que la planification a decide pour un profil.
type planProfil struct {
	elements []planElement
}

type planElement struct {
	id       domain.AVoirID
	termine  bool
	cause    string // pourquoi rien n'est planifie (saisons indisponibles...)
	fichiers []voulu
}

// plan global : les fichiers voulus dans l'ordre, et les contenus vus (a effacer).
type plan struct {
	voulus []voulu
	vus    map[domain.ContenuID]bool
}

// planifier parcourt chaque profil ayant une liste, dans l'ordre de sa liste : un film =
// son fichier ; une serie = ses episodes_avance prochains episodes NON VUS a partir de
// `depuis` sinon de l'episode entame (historique), saisons dans l'ordre. Les profils sont
// parcourus dans un ordre stable (identifiant) : chacun garde l'ordre de sa propre liste.
func (m *Moteur) planifier(ctx context.Context) (*plan, error) {
	profils, err := m.dep.Liste.ProfilsAVoir(ctx)
	if err != nil {
		return nil, err
	}
	p := &plan{vus: map[domain.ContenuID]bool{}}
	deja := map[domain.ContenuID]bool{}
	for _, profil := range profils {
		pp, vus, err := m.planifierProfil(ctx, profil)
		if err != nil {
			m.log.Warn("preparation : planification d'un profil", "erreur", err)
			continue
		}
		for id := range vus {
			p.vus[id] = true
		}
		for _, el := range pp.elements {
			for _, v := range el.fichiers {
				if !deja[v.contenu] {
					deja[v.contenu] = true
					p.voulus = append(p.voulus, v)
				}
			}
		}
	}
	return p, nil
}

// planifierProfil : le plan d'un profil et les contenus qu'il a vus.
func (m *Moteur) planifierProfil(ctx context.Context, profil string) (planProfil, map[domain.ContenuID]bool, error) {
	liste, err := m.dep.Liste.AVoir(ctx, profil)
	if err != nil {
		return planProfil{}, nil, err
	}
	hist, err := m.dep.Etat.Historique(ctx, profil, 0)
	if err != nil {
		return planProfil{}, nil, err
	}
	parContenu := map[domain.ContenuID]domain.Lecture{}
	vus := map[domain.ContenuID]bool{}
	for _, l := range hist {
		if _, ok := parContenu[l.Contenu]; !ok {
			parContenu[l.Contenu] = l // du plus recent au plus ancien
		}
		if l.Vue() {
			vus[l.Contenu] = true
		}
	}
	var pp planProfil
	for _, e := range liste {
		el := planElement{id: e.ID}
		if !e.Serie {
			if parContenu[e.Contenu].Vue() {
				el.termine = true
			} else if v, err := m.voulFilm(ctx, e.Contenu); err != nil {
				el.cause = assainir(err)
			} else {
				el.fichiers = []voulu{v}
			}
			pp.elements = append(pp.elements, el)
			continue
		}
		avance := e.EpisodesAvance
		if avance <= 0 {
			avance = m.r.EpisodesAvance
		}
		fichiers, termine, err := m.voulusSerie(ctx, e, hist, parContenu, avance)
		if err != nil {
			el.cause = assainir(err)
		}
		el.termine, el.fichiers = termine, fichiers
		pp.elements = append(pp.elements, el)
	}
	return pp, vus, nil
}

// voulFilm resout le fichier d'un film par le catalogue (extension) : jamais une URL fournie.
func (m *Moteur) voulFilm(ctx context.Context, id domain.ContenuID) (voulu, error) {
	fid, _, ok := strings.Cut(string(id), ":")
	if !ok || m.srcs[fid] == nil {
		return voulu{}, domain.ErrNonTrouve
	}
	f, err := m.dep.Stockage.Film(ctx, id)
	if err != nil {
		return voulu{}, err
	}
	return voulu{contenu: id, fid: fid, numero: f.Numero, extension: f.Extension}, nil
}

// voulusSerie choisit les `avance` prochains episodes non vus d'une serie.
func (m *Moteur) voulusSerie(ctx context.Context, e domain.ElementAVoir, hist []domain.Lecture, parContenu map[domain.ContenuID]domain.Lecture, avance int) ([]voulu, bool, error) {
	fid, _, ok := strings.Cut(string(e.Contenu), ":")
	if !ok || m.srcs[fid] == nil {
		return nil, false, domain.ErrNonTrouve
	}
	saisons, err := m.saisonsDe(ctx, e.Contenu)
	if err != nil {
		return nil, false, err
	}
	type ep struct {
		domain.Episode
		saison int
	}
	var tous []ep
	for _, s := range saisons {
		for _, x := range s.Episodes {
			tous = append(tous, ep{x, s.Numero})
		}
	}
	if len(tous) == 0 {
		return nil, false, fmt.Errorf("aucun episode connu")
	}
	// Point de depart : `depuis` s'il est donne, sinon l'episode entame le plus recent
	// (apres lui s'il est vu), sinon le premier.
	debut := 0
	switch {
	case e.DepuisSaison > 0:
		debut = len(tous)
		for i, x := range tous {
			if x.saison > e.DepuisSaison || (x.saison == e.DepuisSaison && x.Numero >= e.DepuisEpisode) {
				debut = i
				break
			}
		}
	default:
		for _, l := range hist { // du plus recent au plus ancien
			if l.Serie != e.Contenu {
				continue
			}
			for i, x := range tous {
				if x.ID == l.Contenu {
					debut = i
					if l.Vue() {
						debut = i + 1
					}
					break
				}
			}
			break
		}
	}
	choisir := func(depuis int) []voulu {
		var res []voulu
		for i := depuis; i < len(tous) && len(res) < avance; i++ {
			x := tous[i]
			if parContenu[x.ID].Vue() {
				continue
			}
			numero, ext, _ := strings.Cut(strings.TrimPrefix(string(x.ID), fid+":"), ".")
			res = append(res, voulu{contenu: x.ID, fid: fid, numero: numero, extension: ext, episode: true,
				serie: e.Contenu, saison: x.saison, numEp: x.Numero})
		}
		return res
	}
	res := choisir(debut)
	if len(res) == 0 {
		res = choisir(0) // tout est vu apres le point de depart : ce qui reste avant
	}
	termine := len(res) == 0
	return res, termine, nil
}

// saisonsDe liste les episodes d'une serie (memoire MemoireSaisons : l'appel pese sur
// l'API du fournisseur).
func (m *Moteur) saisonsDe(ctx context.Context, serie domain.ContenuID) ([]domain.Saison, error) {
	m.mu.Lock()
	if s, ok := m.saisons[serie]; ok && m.now().Before(s.jusqua) {
		m.mu.Unlock()
		return s.saisons, nil
	}
	m.mu.Unlock()
	st, err := m.dep.Stockage.Serie(ctx, serie)
	if err != nil {
		return nil, err
	}
	var saisons []domain.Saison
	if m.dep.Saisons != nil {
		saisons, err = m.dep.Saisons(ctx, *st)
	} else {
		saisons, err = m.srcs[st.FournisseurID].Saisons(ctx, st.Numero)
	}
	if err != nil {
		return nil, err
	}
	saisons = append([]domain.Saison(nil), saisons...)
	sort.SliceStable(saisons, func(i, j int) bool { return saisons[i].Numero < saisons[j].Numero })
	for i := range saisons {
		eps := append([]domain.Episode(nil), saisons[i].Episodes...)
		sort.SliceStable(eps, func(a, b int) bool { return eps[a].Numero < eps[b].Numero })
		saisons[i].Episodes = eps
	}
	m.mu.Lock()
	m.saisons[serie] = memoireSaisons{saisons: saisons, jusqua: m.now().Add(m.r.MemoireSaisons)}
	m.mu.Unlock()
	return saisons, nil
}

// appliquer reporte le plan sur l'index : chaque voulu a une entree (attente s'il est
// nouveau), les autres ne sont plus demandes ; ceux qui ont ete vus sont effaces tout de
// suite (modele Netflix : l'episode vu laisse la place au suivant).
func (m *Moteur) appliquer(p *plan) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.index {
		e.demande = false
		e.vu = p.vus[e.Contenu]
	}
	for i, v := range p.voulus {
		e := m.index[v.contenu]
		if e == nil {
			e = &entree{Contenu: v.contenu, Nom: nomFichier(v.contenu, v.episode, v.extension), Etat: domain.PreparationAttente}
			m.index[v.contenu] = e
		}
		e.demande, e.priorite = true, i
		e.Serie, e.Saison, e.Episode = v.serie, v.saison, v.numEp
	}
	for id, e := range m.index {
		if e.demande || !e.vu || e.enLecture > 0 {
			continue
		}
		m.log.Info("preparation : contenu vu, fichier efface", "contenu", string(id), "octets", e.Octets)
		m.effacerLocked(e)
		delete(m.index, id)
		m.inst.Compter("preparation_fichiers_total", 1, "etat", "efface")
	}
	m.sauverIndexLocked()
}

// EtatProfil rend l'etat de preparation de chaque element de la liste d'un profil, d'apres
// une planification faite a l'instant (liste et historique a jour). Un element Termine
// (film vu, serie finie) est a retirer par l'appelant.
func (m *Moteur) EtatProfil(ctx context.Context, profil string) (map[domain.AVoirID]domain.EtatAVoir, error) {
	pp, _, err := m.planifierProfil(ctx, profil)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	res := make(map[domain.AVoirID]domain.EtatAVoir, len(pp.elements))
	for _, el := range pp.elements {
		etat := domain.EtatAVoir{Termine: el.termine, Etat: domain.PreparationAttente}
		if el.cause != "" {
			etat.Etat, etat.Fichiers = domain.PreparationEchec, []domain.FichierPrepare{{Etat: domain.PreparationEchec, Cause: el.cause}}
		}
		for i, v := range el.fichiers {
			f := domain.FichierPrepare{Contenu: v.contenu, Etat: domain.PreparationAttente, Serie: v.serie, Saison: v.saison, Episode: v.numEp}
			if e := m.index[v.contenu]; e != nil {
				f = e.public()
				f.Serie, f.Saison, f.Episode = v.serie, v.saison, v.numEp
			}
			if i == 0 {
				etat.Etat = f.Etat // l'etat de l'element est celui du prochain fichier a lire
			}
			etat.Fichiers = append(etat.Fichiers, f)
		}
		res[el.id] = etat
	}
	return res, nil
}
