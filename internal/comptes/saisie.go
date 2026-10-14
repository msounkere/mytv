package comptes

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"

	"mytv/internal/domain"
)

// LireMotDePasse lit un mot de passe sur une entree (stdin ou fichier) : la
// premiere ligne, sans son retour a la ligne. Un mot de passe en argument de
// ligne de commande finirait dans `ps` et dans l'historique du shell.
func LireMotDePasse(r io.Reader) (string, error) {
	ligne, err := bufio.NewReader(io.LimitReader(r, longueurMax+2)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", errors.New("comptes : lecture du mot de passe impossible")
	}
	ligne = strings.TrimRight(ligne, "\r\n")
	if ligne == "" {
		return "", errors.New("comptes : mot de passe vide")
	}
	return ligne, nil
}

// CreerDepuisEntree cree un utilisateur dont le mot de passe vient d'une entree.
func (s *Service) CreerDepuisEntree(ctx context.Context, nom string, admin bool, r io.Reader) (*domain.Utilisateur, error) {
	mdp, err := LireMotDePasse(r)
	if err != nil {
		return nil, err
	}
	return s.CreerUtilisateur(ctx, nom, mdp, admin)
}
