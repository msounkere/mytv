// Package supervision surveille le compte fournisseur : etat de l'abonnement
// (user_info), echecs de synchronisation, refus d'identifiants ou d'adresse, et
// previent l'administrateur (journal, metrique, notification webhook).
//
// Principes :
//   - l'etat vit en memoire ; il est reconstruit par la synchronisation et par une
//     relecture periodique de user_info (appel API sans flux : il ne prend pas la
//     connexion unique du compte) ;
//   - une notification ne bloque jamais : envoi dans une goroutine bornee, echec
//     journalise sans l'URL ni le secret ;
//   - anti-rafale : au plus un message par evenement et par fournisseur et par
//     periode, selon l'horloge injectee.
package supervision

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"mytv/internal/config"
	"mytv/internal/domain"
)

// Etat est l'etat d'un fournisseur, tel que l'expose l'administration.
type Etat string

const (
	EtatInconnu        Etat = "inconnu" // rien lu depuis le demarrage
	EtatOK             Etat = "ok"
	EtatCompteInvalide Etat = "compte_invalide" // 401/403, abonnement expire, banni
	EtatAdresseRefusee Etat = "adresse_refusee" // 458
	EtatIndisponible   Etat = "indisponible"    // panne, delai, 5xx
)

// Classe range une erreur du fournisseur dans un etat. nil = EtatOK. Les erreurs
// qui ne viennent pas du fournisseur (contexte annule) sont rangees « indisponible »
// par l'appelant, qui les filtre avant.
func Classe(err error) Etat {
	switch {
	case err == nil:
		return EtatOK
	case errors.Is(err, domain.ErrIdentifiants):
		return EtatCompteInvalide
	case errors.Is(err, domain.ErrRefuseIP):
		return EtatAdresseRefusee
	}
	return EtatIndisponible
}

// Code est le code d'erreur de l'API pour un etat d'echec.
func (e Etat) Code() string {
	switch e {
	case EtatCompteInvalide:
		return "fournisseur_compte_invalide"
	case EtatAdresseRefusee:
		return "fournisseur_adresse_refusee"
	}
	return "fournisseur_indisponible"
}

// InfoCompte : ce que publie user_info.
type InfoCompte struct {
	Statut            string
	Expiration        time.Time // zero = illimite ou inconnue
	ConnexionsMax     int
	ConnexionsActives int
}

// Source decrit un fournisseur surveille.
type Source struct {
	ID string
	// AgeMax : age au-dela duquel le catalogue est perime (rafraichissement.catalogue) ;
	// la sante approfondie tolere 2 x AgeMax.
	AgeMax time.Duration
	// Info lit user_info (peut etre nil : fournisseur sans cette API).
	Info func(ctx context.Context) (InfoCompte, error)
}

// Options de Nouveau. Tout est facultatif sauf Config et Sources.
type Options struct {
	Config  config.Supervision
	Sources []Source
	Horloge func() time.Time
	Journal *slog.Logger
	// Jauge publie une jauge Prometheus (telemetrie.Jauge) ; Instrumentation compte les envois.
	Jauge           func(nom string, valeur float64, etiquettes ...string)
	Instrumentation domain.Instrumentation
	// SecretWebhook : valeur DEJA resolue (config.Secret.Valeur). Vide = corps non signe.
	SecretWebhook string
	// ClientHTTP : client des notifications (tests : client TLS du faux serveur).
	ClientHTTP HTTPDoer

	// Dependances de la sante approfondie.
	DerniereSynchro func(ctx context.Context, id string) (time.Time, error)
	Base            func(ctx context.Context) error // la base repond
	Stockage        func() error                    // le stockage est ecrivable
}

type fournisseur struct {
	src Source

	etat         Etat // dernier etat de refus connu (compte_invalide, adresse_refusee) ; vide = aucun
	info         InfoCompte
	infoLue      time.Time
	echecs       int
	erreurCode   string
	erreurQuand  time.Time
	actives      map[string]bool      // conditions d'alerte vraies (pour « retour a la normale »)
	dernierEnvoi map[string]time.Time // anti-rafale, par evenement
	vu           bool                 // au moins une lecture reussie ou un echec depuis le demarrage
}

// Moniteur est la supervision.
type Moniteur struct {
	o       Options
	cfg     config.Supervision
	horloge func() time.Time
	log     *slog.Logger
	inst    domain.Instrumentation

	mu    sync.Mutex
	fs    map[string]*fournisseur
	ordre []string

	notif notifieur
}

// Nouveau cree le moniteur.
func Nouveau(o Options) (*Moniteur, error) {
	if o.Horloge == nil {
		o.Horloge = time.Now
	}
	if o.Journal == nil {
		o.Journal = slog.New(slog.DiscardHandler)
	}
	if o.Instrumentation == nil {
		o.Instrumentation = domain.SansInstrumentation{}
	}
	c := o.Config
	if c.AlerteExpiration <= 0 || c.PeriodeAlerte <= 0 || c.EchecsSynchro < 1 || c.IntervalleInfo <= 0 || c.DelaiSante <= 0 {
		return nil, errors.New("supervision : reglages supervision.* invalides")
	}
	if c.Webhook.URL != "" && c.Webhook.Delai <= 0 {
		return nil, errors.New("supervision : supervision.webhook.delai doit etre > 0")
	}
	m := &Moniteur{o: o, cfg: c, horloge: o.Horloge, log: o.Journal, inst: o.Instrumentation, fs: map[string]*fournisseur{}}
	for _, s := range o.Sources {
		if _, dup := m.fs[s.ID]; dup || s.ID == "" {
			return nil, errors.New("supervision : fournisseur sans identifiant ou en double")
		}
		m.fs[s.ID] = &fournisseur{src: s, actives: map[string]bool{}, dernierEnvoi: map[string]time.Time{}}
		m.ordre = append(m.ordre, s.ID)
	}
	m.notif = nouveauNotifieur(c.Webhook, o.SecretWebhook, o.ClientHTTP, m.log, m.inst)
	return m, nil
}

// Fermer attend les notifications en cours (bornees par leur delai).
func (m *Moniteur) Fermer() { m.notif.fermer() }

// Attendre bloque jusqu'a la fin des notifications en cours (tests).
func (m *Moniteur) Attendre() { m.notif.attendre() }

// Expiration donne la date d'expiration connue de l'abonnement.
func (m *Moniteur) Expiration(id string) (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f := m.fs[id]
	if f == nil || f.info.Expiration.IsZero() {
		return time.Time{}, false
	}
	return f.info.Expiration, true
}

// Synchro est appele par le synchroniseur apres chaque tentative (catalogue.Observateur).
// Apres un succes, user_info est relu : un abonnement qui expire est vu avant qu'il casse.
func (m *Moniteur) Synchro(ctx context.Context, id string, err error) {
	m.mu.Lock()
	f := m.fs[id]
	if f == nil {
		m.mu.Unlock()
		return
	}
	f.vu = true
	if err != nil {
		if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
			m.mu.Unlock()
			return // annulation voulue (arret) : pas une panne
		}
		f.echecs++
		m.noterErreurLocked(f, Classe(err))
	} else {
		f.echecs = 0
		f.etat = ""
	}
	m.mu.Unlock()
	if err == nil {
		_ = m.rafraichir(ctx, f)
		return
	}
	m.evaluer(id)
}

// SignalerRefus note qu'une requete de lecture a ete refusee par le fournisseur
// (identifiants ou adresse). Les autres erreurs sont ignorees : une panne passagere
// d'un segment n'est pas un etat du compte.
func (m *Moniteur) SignalerRefus(id string, err error) {
	c := Classe(err)
	if c != EtatCompteInvalide && c != EtatAdresseRefusee {
		return
	}
	m.mu.Lock()
	f := m.fs[id]
	if f == nil {
		m.mu.Unlock()
		return
	}
	f.vu = true
	m.noterErreurLocked(f, c)
	m.mu.Unlock()
	m.evaluer(id)
}

// noterErreurLocked : verrou tenu.
func (m *Moniteur) noterErreurLocked(f *fournisseur, c Etat) {
	f.erreurCode = c.Code()
	f.erreurQuand = m.horloge()
	if c == EtatCompteInvalide || c == EtatAdresseRefusee {
		f.etat = c
	}
}

// RafraichirInfo relit user_info de chaque fournisseur (appel API, aucun flux).
func (m *Moniteur) RafraichirInfo(ctx context.Context) {
	for _, id := range m.ordre {
		_ = m.rafraichir(ctx, m.fs[id])
	}
}

// rafraichir relit user_info et rend l'erreur de la lecture (nil = lue).
func (m *Moniteur) rafraichir(ctx context.Context, f *fournisseur) error {
	if f.src.Info == nil {
		m.evaluer(f.src.ID)
		return nil
	}
	ctx, annuler := context.WithTimeout(ctx, m.cfg.DelaiSante.Std())
	defer annuler()
	info, err := f.src.Info(ctx)
	m.mu.Lock()
	switch {
	case err == nil:
		f.info, f.infoLue, f.vu = info, m.horloge(), true
		f.etat = ""
	case ctx.Err() != nil && errors.Is(err, ctx.Err()) && ctx.Err() != context.DeadlineExceeded:
		m.mu.Unlock()
		return err // arret demande
	default:
		f.vu = true
		m.noterErreurLocked(f, Classe(err))
	}
	m.mu.Unlock()
	m.evaluer(f.src.ID)
	return err
}

// Planifier relit user_info au demarrage puis toutes les supervision.intervalle_info,
// jusqu'a l'annulation de ctx.
func (m *Moniteur) Planifier(ctx context.Context) {
	m.RafraichirInfo(ctx)
	t := time.NewTicker(m.cfg.IntervalleInfo.Std())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.RafraichirInfo(ctx)
		}
	}
}

// etatLocked derive l'etat courant. Verrou tenu.
func (m *Moniteur) etatLocked(f *fournisseur, maintenant time.Time) Etat {
	switch {
	case f.etat != "":
		return f.etat
	case !f.infoLue.IsZero() && !statutActif(f.info.Statut):
		return EtatCompteInvalide
	case !f.info.Expiration.IsZero() && !f.info.Expiration.After(maintenant):
		return EtatCompteInvalide
	case f.echecs > 0:
		return EtatIndisponible
	case !f.vu:
		return EtatInconnu
	}
	return EtatOK
}

func statutActif(s string) bool { return s == "" || strings.EqualFold(s, "active") }

// Evenements notifiables.
const (
	evEchecSynchro     = "echec_synchro"
	evExpirationProche = "expiration_proche"
	evCompteRefuse     = "compte_refuse"
	evRetourNormal     = "retour_normal"
)

// evaluer met la jauge a jour, calcule les conditions d'alerte et emet (journal et
// notification) ce que l'anti-rafale permet. Aucun verrou n'est tenu pendant l'envoi.
func (m *Moniteur) evaluer(id string) {
	var envois []evenement
	m.mu.Lock()
	f := m.fs[id]
	now := m.horloge()
	etat := m.etatLocked(f, now)
	restant := time.Duration(0)
	expConnue := !f.info.Expiration.IsZero()
	if expConnue {
		restant = f.info.Expiration.Sub(now)
		if m.o.Jauge != nil {
			m.o.Jauge("fournisseur_expire_dans_secondes", restant.Seconds(), "fournisseur", id)
		}
	}
	conds := map[string]bool{
		evCompteRefuse:     etat == EtatCompteInvalide || etat == EtatAdresseRefusee,
		evExpirationProche: etat == EtatOK && expConnue && restant > 0 && restant <= m.cfg.AlerteExpiration.Std(),
		evEchecSynchro:     etat == EtatIndisponible && f.echecs >= m.cfg.EchecsSynchro,
	}
	emet := func(nom string, e evenement) {
		e.Nom, e.Fournisseur, e.Quand = nom, id, now
		if last, ok := f.dernierEnvoi[nom]; ok && now.Sub(last) < m.cfg.PeriodeAlerte.Std() {
			return // anti-rafale
		}
		f.dernierEnvoi[nom] = now
		envois = append(envois, e)
	}
	for _, nom := range []string{evCompteRefuse, evExpirationProche, evEchecSynchro} {
		if !conds[nom] {
			continue
		}
		f.actives[nom] = true
		e := evenement{EchecsConsecutifs: f.echecs}
		if expConnue {
			exp, sec := f.info.Expiration.UTC(), int64(restant.Seconds())
			e.Expiration, e.ExpireDansSecondes = &exp, &sec
		}
		switch nom {
		case evCompteRefuse:
			e.Message = "le fournisseur refuse le compte (" + raison(etat, expConnue && restant <= 0) + ")"
		case evExpirationProche:
			e.Message = "l'abonnement du fournisseur expire bientot"
		case evEchecSynchro:
			e.Message = "la synchronisation du catalogue echoue de facon repetee"
		}
		emet(nom, e)
	}
	toutesFausses := true
	for _, v := range conds {
		if v {
			toutesFausses = false
		}
	}
	if toutesFausses && len(f.actives) > 0 {
		clear(f.actives)
		emet(evRetourNormal, evenement{Message: "le fournisseur est revenu a la normale"})
	}
	m.mu.Unlock()
	for _, e := range envois {
		// Le journal ne porte que des champs fixes : jamais d'URL ni de secret.
		m.log.Warn("supervision : "+e.Message, "evenement", e.Nom, "fournisseur", id,
			"echecs_consecutifs", e.EchecsConsecutifs, "expire_dans", dureeLisible(e.ExpireDansSecondes))
		m.notif.envoyer(e)
	}
}

func raison(e Etat, expire bool) string {
	switch {
	case e == EtatAdresseRefusee:
		return "adresse refusee"
	case expire:
		return "abonnement expire"
	}
	return "identifiants ou abonnement refuses"
}

func dureeLisible(sec *int64) string {
	if sec == nil {
		return ""
	}
	return (time.Duration(*sec) * time.Second).Round(time.Minute).String()
}
