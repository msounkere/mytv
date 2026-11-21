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
	// Fuseau : server_info.timezone (« Europe/Amsterdam »), vide si absent. Les heures
	// de debut du replay (timeshift) s'expriment dans ce fuseau.
	Fuseau string
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
		ServerInfo *struct {
			Fuseau texte `json:"timezone"`
		} `json:"server_info"`
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
	info := InfoCompte{
		Statut: u.Statut.String(), Expiration: u.Expiration.Heure(),
		ConnexionsMax: int(u.ConnexionsMax), ConnexionsActives: int(u.ConnexionsActi),
	}
	if corps.ServerInfo != nil {
		info.Fuseau = corps.ServerInfo.Fuseau.String()
	}
	return info, nil
}

// FuseauServeur rend le fuseau du serveur du fournisseur (server_info.timezone), lu dans
// user_info (aucun flux) et garde 24 h. Fuseau absent ou inconnu : erreur, jamais UTC par
// defaut (une heure decalee servirait un autre programme sans que rien ne le signale).
func (c *Client) FuseauServeur(ctx context.Context) (*time.Location, error) {
	c.fuseauMu.Lock()
	defer c.fuseauMu.Unlock()
	if c.fuseau != nil && c.horloge().Sub(c.fuseauLu) < 24*time.Hour {
		return c.fuseau, nil
	}
	info, err := c.InfoCompte(ctx)
	if err != nil {
		return nil, err
	}
	if info.Fuseau == "" {
		return nil, fmt.Errorf("%w : fuseau du serveur absent de server_info", domain.ErrAmont)
	}
	loc, err := time.LoadLocation(info.Fuseau)
	if err != nil {
		return nil, fmt.Errorf("%w : fuseau du serveur inconnu (%q)", domain.ErrAmont, info.Fuseau)
	}
	c.fuseau, c.fuseauLu = loc, c.horloge()
	return loc, nil
}
