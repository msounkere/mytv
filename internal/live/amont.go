package live

import (
	"context"
	"errors"
	"net/http"
	"time"

	"mytv/internal/domain"
)

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
//     on PATIENTE jusqu'a bail.patience_max en retentant toutes les bail.reessai, sans
//     consommer d'essai, et on renouvelle le bail pour ne pas le perdre en attendant.
func (r *Relais) avecEssais(ctx context.Context, essais int, renouveler func(), essai func(ctx context.Context) error) error {
	debut, ok := ctx.Value(cleDebut{}).(time.Time)
	if !ok {
		debut = r.opt.Horloge()
	}
	limite := debut.Add(r.opt.Bail.PatienceMax.Std())
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
			if r.opt.Horloge().Add(r.opt.Bail.Reessai.Std()).After(limite) {
				return err
			}
			r.st.incrementer(&r.st.patiences)
			r.inst.Compter("live_patiences_407_total", 1)
			if renouveler != nil {
				renouveler()
			}
			if e := attendre(ctx, r.opt.Bail.Reessai.Std()); e != nil {
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
