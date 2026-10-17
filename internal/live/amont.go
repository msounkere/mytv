package live

import (
	"context"
	"errors"
	"net/http"
	"time"

	"mytv/internal/baux"
	"mytv/internal/domain"
	"mytv/internal/occupation"
)

// patience : la patience sur 407 est partagee avec vod (paquet baux).
func (r *Relais) patience() baux.Patience {
	p := baux.PatienceDepuisConfig(r.opt.Bail, r.opt.Horloge)
	p.Apres, p.Aleatoire = r.opt.Apres, r.opt.Aleatoire
	return p
}

// demande identifie, pour le diagnostic d'occupation, la lecture d'une chaine par une session.
func (r *Relais) demande(ctx context.Context, chaine string) occupation.Demande {
	session, _ := ctx.Value(cleSession{}).(string)
	return occupation.Demande{Fournisseur: r.opt.Fournisseur.ID(), Session: session, Ressource: chaine, Direct: true}
}

// requete fait UNE requete amont, bornee par le semaphore (le fournisseur n'a
// qu'une connexion) et par un delai qui couvre aussi la lecture du corps. `fin`
// libere les deux : l'appelant doit l'appeler apres avoir ferme le corps.
func (r *Relais) requete(ctx context.Context, cible string, entetes http.Header, delai time.Duration) (*http.Response, context.CancelFunc, error) {
	select {
	case r.amont <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	ctx2, annuler := context.WithTimeout(ctx, delai)
	rep, err := r.opt.Fournisseur.Requete(ctx2, cible, entetes)
	if err != nil {
		annuler()
		<-r.amont
		return nil, nil, err
	}
	return rep, func() { annuler(); <-r.amont }, nil
}

// attendre dort `d` ou jusqu'a l'annulation du contexte.
func attendre(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// avecEssais execute `essai` jusqu'a `essais` fois (pause `attenteEssais` entre deux).
//
// Deux cas ne comptent pas comme un echec ordinaire :
//   - identifiants refuses ou contenu introuvable : un deuxieme essai ne sera pas
//     meilleur, on renonce tout de suite ;
//   - 407 (la place du fournisseur est encore gardee apres un zapping, ~100 s) :
//     on PATIENTE jusqu'a bail.patience_max, sans consommer d'essai, en espacant de plus en
//     plus les requetes (bail.reessai, double a chaque refus, plafond bail.reessai_max) pour ne
//     pas marteler le fournisseur, et on renouvelle le bail pour ne pas le perdre en attendant.
//
// Au deuxieme refus d'une meme demande, le 407 « persiste » : l'occupation du compte est
// diagnostiquee (une lecture de user_info, mise en cache). Patience epuisee, l'erreur rendue
// porte cette classification (occupation.ErrOccupe) pour que la reponse 503 la nomme.
func (r *Relais) avecEssais(ctx context.Context, chaine string, essais int, renouveler func(), essai func(ctx context.Context) error) error {
	debut, ok := ctx.Value(cleDebut{}).(time.Time)
	if !ok {
		debut = r.opt.Horloge()
	}
	d := r.demande(ctx, chaine)
	refus := 0
	classe := occupation.Occupe
	for tentative := 1; ; {
		err := essai(ctx)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		switch {
		case errors.Is(err, domain.ErrIdentifiants), errors.Is(err, domain.ErrIntrouvable):
			return err
		case errors.Is(err, domain.ErrRefuseConnexions):
			refus++
			switch refus {
			case 1:
				r.opt.Occupation.Refus(d, "407") // coupure en pleine lecture ? (journal, metrique)
			case 2:
				classe = r.opt.Occupation.Diagnostiquer(ctx, d)
			}
			if e := r.patience().Attendre(ctx, debut, err, refus-1, func() {
				r.st.incrementer(&r.st.patiences)
				r.inst.Compter("live_patiences_407_total", 1)
				if renouveler != nil {
					renouveler()
				}
			}); e != nil {
				if e == err { // patience epuisee (et non contexte annule)
					return &occupation.ErrOccupe{Classe: classe, Err: err}
				}
				return e
			}
			continue
		}
		if tentative >= essais {
			return err
		}
		tentative++
		r.st.incrementer(&r.st.essais)
		r.inst.Compter("live_nouveaux_essais_total", 1)
		if e := attendre(ctx, r.opt.Reseau.AttenteEssais.Std()); e != nil {
			return e
		}
	}
}
