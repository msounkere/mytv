package appli

import (
	"errors"
	"fmt"
	"os"
	"time"

	"mytv/internal/config"
)

// SecretSigneMin : longueur minimale (octets) de serveur.secret_signature. Il signe
// et chiffre les URL de segments : un secret court se devine par force brute.
const SecretSigneMin = 32

// Verifier controle une configuration deja chargee (valeurs par defaut et
// validation faites par config.Lire) et rassemble TOUTES les erreurs de demarrage :
// secrets introuvables, secret de signature trop court, dossier d'interface absent,
// fuseau inconnu. Il ne contacte personne et n'ouvre rien : c'est aussi ce que fait
// `mytv --verifier-config`.
func Verifier(c *config.Config) error {
	var errs []error
	if err := c.Resoudre(); err != nil {
		errs = append(errs, err)
	}
	// Une erreur de lecture est deja rapportee par Resoudre ; on ne controle la
	// longueur que d'un secret lisible.
	if s, err := c.Serveur.SecretSigne.Valeur(); err == nil && len(s) < SecretSigneMin {
		errs = append(errs, fmt.Errorf("serveur.secret_signature fait %d octets, %d au minimum", len(s), SecretSigneMin))
	}
	if d := c.Serveur.UIDossier; d != "" {
		if st, err := os.Stat(d); err != nil || !st.IsDir() {
			errs = append(errs, fmt.Errorf("serveur.ui_dossier %q n'est pas un dossier lisible", d))
		}
	}
	if _, err := time.LoadLocation(c.EPG.Fuseau); err != nil {
		errs = append(errs, fmt.Errorf("epg.fuseau %q inconnu", c.EPG.Fuseau))
	}
	return errors.Join(errs...)
}
