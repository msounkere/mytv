package occupation

import (
	"context"
	"errors"
)

// ErrOccupe est l'erreur d'un refus 407 persistant, avec sa classification. Elle enveloppe
// l'erreur d'origine : errors.Is(err, domain.ErrRefuseConnexions) reste vrai pour tous ceux
// qui ne s'interessent qu'a « le fournisseur est occupe ».
type ErrOccupe struct {
	Classe Classe
	Err    error
}

func (e *ErrOccupe) Error() string { return string(e.Classe) + " : " + e.Err.Error() }
func (e *ErrOccupe) Unwrap() error { return e.Err }

// ClasseDe extrait la classification portee par une erreur ; ok=faux si elle n'en porte pas.
func ClasseDe(err error) (Classe, bool) {
	var e *ErrOccupe
	if errors.As(err, &e) {
		return e.Classe, true
	}
	return "", false
}

// Diagnostiquer classe l'occupation du compte pour une demande refusee.
//
// Ordre, du moins couteux au plus couteux (le dernier seul fait un appel reseau) :
//  1. une autre session de mytv tient un bail : ParMytv ;
//  2. mytv vient lui-meme d'utiliser le compte (autre ressource, moins de
//     bail.garde_fournisseur) : le fournisseur garde SA place, Occupe ;
//  3. user_info : toutes les places prises alors qu'aucune n'est a mytv -> Ailleurs ;
//     sinon, ou si la lecture echoue, Occupe.
func (s *Suivi) Diagnostiquer(ctx context.Context, d Demande) Classe {
	if s == nil {
		return Occupe
	}
	src := s.srcs[d.Fournisseur]
	if src.AutresBaux != nil && src.AutresBaux(d.Session) > 0 {
		return ParMytv
	}
	if s.placeGardeeParMytv(d) {
		return Occupe
	}
	info, err := s.lireInfo(ctx, d.Fournisseur)
	if err != nil || info.ConnexionsMax < 1 || info.ConnexionsActives < info.ConnexionsMax {
		return Occupe
	}
	s.marquerAilleurs(d.Fournisseur)
	return Ailleurs
}

func (s *Suivi) placeGardeeParMytv(d Demande) bool {
	now := s.horloge()
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.etat(d.Fournisseur).dernier
	if u.quand.IsZero() || now.Sub(u.quand) > s.r.GardeFournisseur {
		return false
	}
	// Meme chaine du direct : c'est la meme connexion, que quelqu'un d'autre a prise.
	if d.Direct && u.ressource == d.Ressource && u.session == d.Session {
		return false
	}
	return true
}

// marquerAilleurs : le diagnostic a vu le compte pris sans que mytv y soit pour rien.
func (s *Suivi) marquerAilleurs(f string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.etat(f)
	e.ailleursVu = s.horloge()
	if e.ailleursDe.IsZero() {
		e.ailleursDe = e.ailleursVu
	}
}

// lireInfo rend user_info depuis le cache ou, au plus une fois par supervision.cache_occupation
// et par fournisseur, depuis le fournisseur. Les appels simultanes partagent la meme requete
// (singleflight) ; un echec est aussi mis en cache : on ne reinterroge pas un fournisseur
// qui vient de ne pas repondre. La requete vit autant que le suivi, pas que le client qui
// l'a declenchee, car les autres l'attendent.
func (s *Suivi) lireInfo(ctx context.Context, f string) (Info, error) {
	src := s.srcs[f]
	if src.Info == nil {
		return Info{}, errors.New("occupation : user_info non disponible")
	}
	s.mu.Lock()
	e := s.etat(f)
	if !e.lueLe.IsZero() && s.horloge().Sub(e.lueLe) < s.r.CacheOccupation {
		info, err := e.info, e.err
		s.mu.Unlock()
		return info, err
	}
	a := e.enVol
	if a == nil {
		a = &appel{fini: make(chan struct{})}
		e.enVol = a
		s.taches.Add(1)
		go s.interroger(f, src, a)
	}
	s.mu.Unlock()
	select {
	case <-a.fini:
		return a.info, a.err
	case <-ctx.Done():
		return Info{}, ctx.Err()
	}
}

func (s *Suivi) interroger(f string, src Source, a *appel) {
	defer s.taches.Done()
	ctx, annuler := context.WithTimeout(s.base, s.r.DelaiInfo)
	defer annuler()
	a.info, a.err = src.Info(ctx)
	s.inst.Compter("occupation_lectures_user_info_total", 1, "fournisseur", f)
	s.mu.Lock()
	e := s.etat(f)
	e.info, e.err, e.lueLe = a.info, a.err, s.horloge()
	e.enVol = nil
	s.mu.Unlock()
	close(a.fini)
}
