package xtream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"mytv/internal/domain"
)

// InfoCompte est l'etat du compte tel que le fournisseur le publie dans
// `user_info` (player_api.php sans action). L'appel ne demande aucun flux : il ne
// prend pas la connexion unique du compte.
type InfoCompte struct {
	// Statut : « Active », « Expired », « Banned », « Disabled »... tel que publie.
	Statut string
	// Expiration : zero quand le fournisseur n'en donne pas (abonnement illimite ou inconnu).
	Expiration time.Time
	// ConnexionsMax et ConnexionsActives : 0 si absents. active_cons est inexact chez
	// le fournisseur reel (toujours 0) : a afficher, jamais a croire.
	ConnexionsMax     int
	ConnexionsActives int
}

// InfoCompte lit l'etat du compte. Un refus d'identifiants (401/403 ou `auth: 0`)
// et un refus d'adresse (458) sont rendus comme erreurs typees.
func (c *Client) InfoCompte(ctx context.Context) (InfoCompte, error) {
	rep, err := c.Requete(ctx, c.urlAPI(""), nil)
	if err != nil {
		return InfoCompte{}, err
	}
	defer rep.Body.Close()
	var corps struct {
		UserInfo *struct {
			Auth           texte   `json:"auth"`
			Statut         texte   `json:"status"`
			Expiration     instant `json:"exp_date"`
			ConnexionsMax  entier  `json:"max_connections"`
			ConnexionsActi entier  `json:"active_cons"`
		} `json:"user_info"`
	}
	if err := json.NewDecoder(io.LimitReader(rep.Body, reponseErreurMax)).Decode(&corps); err != nil {
		if ctx.Err() != nil {
			return InfoCompte{}, ctx.Err()
		}
		return InfoCompte{}, fmt.Errorf("%w : user_info illisible", domain.ErrAmont)
	}
	u := corps.UserInfo
	if u == nil {
		return InfoCompte{}, fmt.Errorf("%w : user_info absent", domain.ErrAmont)
	}
	if u.Auth == "0" {
		return InfoCompte{}, domain.ErrIdentifiants
	}
	return InfoCompte{
		Statut: u.Statut.String(), Expiration: u.Expiration.Heure(),
		ConnexionsMax: int(u.ConnexionsMax), ConnexionsActives: int(u.ConnexionsActi),
	}, nil
}
