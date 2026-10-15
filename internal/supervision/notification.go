package supervision

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"mytv/internal/config"
	"mytv/internal/domain"
)

// HTTPDoer est ce que la notification demande a un client HTTP.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// envoisMax borne les notifications simultanees : un webhook lent ne doit pas faire
// grossir le nombre de goroutines. Au-dela, la notification est abandonnee (et comptee).
const envoisMax = 4

// evenement est le corps JSON envoye au webhook. Aucun identifiant fournisseur
// (utilisateur, mot de passe) : seulement l'identifiant de configuration.
type evenement struct {
	Nom                string     `json:"evenement"`
	Fournisseur        string     `json:"fournisseur"`
	Quand              time.Time  `json:"horodatage"`
	Message            string     `json:"message"`
	Expiration         *time.Time `json:"expiration,omitempty"`
	ExpireDansSecondes *int64     `json:"expire_dans_secondes,omitempty"`
	EchecsConsecutifs  int        `json:"echecs_consecutifs,omitempty"`
}

type notifieur struct {
	url     string
	secret  []byte
	delai   time.Duration
	client  HTTPDoer
	log     *slog.Logger
	inst    domain.Instrumentation
	slots   chan struct{}
	wg      sync.WaitGroup
	annuler context.CancelFunc
	ctx     context.Context
}

func nouveauNotifieur(w config.Webhook, secret string, client HTTPDoer, log *slog.Logger, inst domain.Instrumentation) notifieur {
	n := notifieur{url: w.URL, delai: w.Delai.Std(), log: log, inst: inst, slots: make(chan struct{}, envoisMax)}
	if secret != "" {
		n.secret = []byte(secret)
	}
	if client == nil {
		// Pas de redirection suivie : un webhook compromis ne doit pas nous faire joindre
		// autre chose, ni rejouer le secret ailleurs.
		client = &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport: &http.Transport{
				DialContext:         (&net.Dialer{Timeout: w.Delai.Std()}).DialContext,
				TLSHandshakeTimeout: w.Delai.Std(),
				DisableKeepAlives:   true,
			},
		}
	}
	n.client = client
	n.ctx, n.annuler = context.WithCancel(context.Background())
	return n
}

// envoyer ne bloque jamais l'appelant.
func (n *notifieur) envoyer(e evenement) {
	if n.url == "" {
		return
	}
	select {
	case n.slots <- struct{}{}:
	default:
		n.inst.Compter("supervision_webhook_total", 1, "resultat", "abandon")
		n.log.Warn("supervision : notification abandonnee, trop d'envois en cours", "evenement", e.Nom)
		return
	}
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		defer func() { <-n.slots }()
		n.poster(e)
	}()
}

func (n *notifieur) poster(e evenement) {
	corps, _ := json.Marshal(e)
	ctx, annuler := context.WithTimeout(n.ctx, n.delai)
	defer annuler()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.url, bytes.NewReader(corps))
	if err != nil {
		n.echec(e, "requete_invalide")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "mytv-supervision")
	req.Header.Set("X-Mytv-Evenement", e.Nom)
	if len(n.secret) > 0 {
		h := hmac.New(sha256.New, n.secret)
		h.Write(corps)
		req.Header.Set("X-Mytv-Signature", "sha256="+hex.EncodeToString(h.Sum(nil)))
	}
	rep, err := n.client.Do(req)
	if err != nil {
		// err.Error() citerait l'URL complete (net/http) : on n'en journalise que la nature.
		cause := "reseau"
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			cause = "delai_depasse"
		case errors.Is(err, context.Canceled):
			cause = "annule"
		}
		n.echec(e, cause)
		return
	}
	defer rep.Body.Close()
	if rep.StatusCode < 200 || rep.StatusCode >= 300 {
		n.echec(e, "statut_"+strconv.Itoa(rep.StatusCode))
		return
	}
	n.inst.Compter("supervision_webhook_total", 1, "resultat", "ok")
}

func (n *notifieur) echec(e evenement, cause string) {
	n.inst.Compter("supervision_webhook_total", 1, "resultat", "echec")
	n.log.Warn("supervision : notification non envoyee", "evenement", e.Nom, "cause", cause)
}

func (n *notifieur) attendre() { n.wg.Wait() }

func (n *notifieur) fermer() {
	n.annuler()
	n.wg.Wait()
}
