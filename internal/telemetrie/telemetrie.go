package telemetrie

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"mytv/internal/config"
	"mytv/internal/domain"
)

// Options de Nouveau.
type Options struct {
	Config  config.Diagnostic
	Horloge func() time.Time // nil = time.Now
	// Authentifier rend l'identifiant STABLE de la session (comptes.Service.Authentifier) ;
	// EstAdmin dit si la requete porte une session d'administrateur. Sans eux, tout est refuse.
	Authentifier func(*http.Request) (string, bool)
	EstAdmin     func(*http.Request) bool
	Journal      *slog.Logger
}

// Telemetrie rassemble le registre de metriques, le journal d'evenements et les routes.
type Telemetrie struct {
	o        Options
	actif    bool
	reg      *registre
	jour     *journal
	limiteur *limiteur
	log      *slog.Logger
}

func Nouveau(o Options) (*Telemetrie, error) {
	if o.Horloge == nil {
		o.Horloge = time.Now
	}
	if o.Journal == nil {
		o.Journal = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	t := &Telemetrie{o: o, actif: o.Config.Actif, log: o.Journal}
	if !t.actif {
		return t, nil // rien n'est cree : ni dossier, ni registre
	}
	c := o.Config
	if c.EnvoiMax < 1 || c.EvenementsParEnvoi < 1 || c.DebitEvenements < 1 || c.AppareilsMax < 1 || c.TexteMax < 1 || c.SeriesMax < 1 {
		return nil, errors.New("telemetrie : reglages diagnostic.* invalides (tous doivent etre > 0)")
	}
	j, err := ouvrirJournal(reglagesJournal{Dossier: c.Dossier, Conservation: c.Conservation.Std(), SegmentMax: c.SegmentMax, TailleMax: c.TailleMax}, o.Horloge)
	if err != nil {
		return nil, err
	}
	t.jour = j
	t.reg = nouveauRegistre(c.SeriesMax, nil)
	t.limiteur = nouveauLimiteur(c.DebitEvenements, c.AppareilsMax, o.Horloge)
	return t, nil
}

// Actif : le diagnostic est-il en marche (diagnostic.actif).
func (t *Telemetrie) Actif() bool { return t.actif }

// Instrumentation est ce que l'on donne aux modules. Inactif : SansInstrumentation,
// dont les methodes ne font rien (cout nul).
func (t *Telemetrie) Instrumentation() domain.Instrumentation {
	if !t.actif {
		return domain.SansInstrumentation{}
	}
	return instrumentation{t}
}

// Jauge pose la valeur d'une jauge Prometheus. Elle n'est pas dans
// domain.Instrumentation (contrat gele) : seul l'assemblage la branche, a ceux qui
// en ont besoin (supervision). Sans effet quand le diagnostic est inactif.
func (t *Telemetrie) Jauge(nom string, valeur float64, etiquettes ...string) {
	if t.actif {
		t.reg.fixer(nom, valeur, etiquettes)
	}
}

// Fermer ferme le segment ouvert.
func (t *Telemetrie) Fermer() error {
	if !t.actif {
		return nil
	}
	return t.jour.fermerTout()
}

type instrumentation struct{ t *Telemetrie }

func (i instrumentation) Compter(nom string, delta float64, e ...string) {
	i.t.reg.compter(nom, delta, e)
}
func (i instrumentation) Observer(nom string, v float64, e ...string) {
	i.t.reg.observer(nom, v, e)
}

var reType = regexp.MustCompile(`^[a-z][a-z0-9_]*(:[a-z0-9_.\-]+)*$`)

// Evenement enregistre un evenement du serveur (message masque et borne).
func (i instrumentation) Evenement(type_, message string) {
	t := i.t
	if !reType.MatchString(type_) || len(type_) > 40 {
		return
	}
	err := t.jour.ecrire([]Evenement{{
		Recu: t.o.Horloge().UTC(), Source: "serveur", Type: type_,
		Message: tronquer(Masquer(message), t.o.Config.TexteMax),
	}})
	if err != nil {
		t.log.Warn("diagnostic : ecriture impossible", "erreur", err)
	}
}

// empreinte : l'identifiant d'appareil choisi par le client ne sert qu'a regrouper
// les evenements d'une lecture ; on n'en garde qu'une empreinte courte.
func empreinte(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:4])
}
