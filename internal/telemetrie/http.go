package telemetrie

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Chemins relatifs des routes (l'API les monte sous son prefixe).
const (
	CheminEvenements = "/diagnostic/evenements" // POST (tout utilisateur connecte), GET (administrateur)
	CheminMetriques  = "/metriques"             // GET (administrateur)
)

// Routes rend un routeur des trois routes.
func (t *Telemetrie) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST "+CheminEvenements, http.HandlerFunc(t.Recevoir))
	mux.Handle("GET "+CheminEvenements, http.HandlerFunc(t.Lire))
	mux.Handle("GET "+CheminMetriques, http.HandlerFunc(t.Metriques))
	return mux
}

// erreur ecrit l'erreur normalisee {code, message} ; les codes sont ceux de l'API
// (docs/openapi.yaml), stables et lisibles par une machine.
func erreur(w http.ResponseWriter, statut int, code, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(statut)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "message": msg})
}

// inactif : diagnostic.actif = false. Meme reponse que l'API pour une fonction non activee.
func inactif(w http.ResponseWriter) {
	erreur(w, http.StatusNotImplemented, "non_disponible", "diagnostic inactif")
}

// admin : refus par defaut. 401 sans session, 403 pour un utilisateur ordinaire.
func (t *Telemetrie) admin(w http.ResponseWriter, req *http.Request) bool {
	if t.o.Authentifier == nil || t.o.EstAdmin == nil {
		erreur(w, http.StatusForbidden, "interdit", "acces refuse")
		return false
	}
	if _, ok := t.o.Authentifier(req); !ok {
		erreur(w, http.StatusUnauthorized, "non_authentifie", "authentification requise")
		return false
	}
	if !t.o.EstAdmin(req) {
		erreur(w, http.StatusForbidden, "interdit", "reserve aux administrateurs")
		return false
	}
	return true
}

// Metriques expose le format Prometheus (administrateurs).
func (t *Telemetrie) Metriques(w http.ResponseWriter, req *http.Request) {
	if !t.admin(w, req) {
		return
	}
	if !t.actif {
		inactif(w)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = t.reg.ecrire(w)
}

// Lire renvoie les evenements recents, du plus recent au plus ancien (administrateurs).
func (t *Telemetrie) Lire(w http.ResponseWriter, req *http.Request) {
	if !t.admin(w, req) {
		return
	}
	if !t.actif {
		inactif(w)
		return
	}
	lim, _ := strconv.Atoi(req.URL.Query().Get("limite"))
	typ := req.URL.Query().Get("type")
	if len(typ) > 40 {
		erreur(w, http.StatusBadRequest, "requete_invalide", "type trop long")
		return
	}
	evs, err := t.jour.lire(Filtre{Type: typ, Limite: lim})
	if err != nil {
		erreur(w, http.StatusInternalServerError, "erreur_interne", "lecture impossible")
		return
	}
	if evs == nil {
		evs = []Evenement{}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"elements": evs})
}

// Recevoir enregistre les evenements envoyes par l'interface.
//
// Limites : session valide ; Content-Type JSON (une page d'un autre site ne peut
// pas l'envoyer sans autorisation CORS) ; corps <= diagnostic.envoi_max ;
// <= evenements_par_envoi evenements ; debit par session (seau a jetons) ; chaque
// evenement est reduit a des types simples, masque et borne.
func (t *Telemetrie) Recevoir(w http.ResponseWriter, req *http.Request) {
	if t.o.Authentifier == nil {
		erreur(w, http.StatusForbidden, "interdit", "acces refuse")
		return
	}
	session, ok := t.o.Authentifier(req)
	if !ok || session == "" {
		erreur(w, http.StatusUnauthorized, "non_authentifie", "authentification requise")
		return
	}
	if !t.actif {
		inactif(w)
		return
	}
	if mt, _, err := mime.ParseMediaType(req.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		erreur(w, http.StatusUnsupportedMediaType, "format_non_json", "application/json attendu")
		return
	}
	c := t.o.Config
	if req.ContentLength > c.EnvoiMax {
		erreur(w, http.StatusRequestEntityTooLarge, "corps_trop_gros", "envoi trop volumineux")
		return
	}
	corps, err := io.ReadAll(http.MaxBytesReader(w, req.Body, c.EnvoiMax))
	if err != nil {
		var tg *http.MaxBytesError
		if errors.As(err, &tg) {
			erreur(w, http.StatusRequestEntityTooLarge, "corps_trop_gros", "envoi trop volumineux")
		} else {
			erreur(w, http.StatusBadRequest, "requete_invalide", "corps illisible")
		}
		return
	}
	var env struct {
		Appareil  string `json:"appareil"`
		Agent     string `json:"agent"`
		Evenement []any  `json:"evenements"`
	}
	if err := json.Unmarshal(corps, &env); err != nil || env.Evenement == nil {
		erreur(w, http.StatusBadRequest, "requete_invalide", "JSON invalide : champ « evenements » attendu")
		return
	}
	offerts := env.Evenement
	if len(offerts) > c.EvenementsParEnvoi {
		offerts = offerts[:c.EvenementsParEnvoi]
	}
	permis, attente := t.limiteur.prendre(session, len(offerts))
	if permis == 0 && len(offerts) > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(attente/time.Second)+1))
		erreur(w, http.StatusTooManyRequests, "trop_de_requetes", "trop d'evenements, ralentir")
		return
	}
	now := t.o.Horloge().UTC()
	appareil := empreinte(env.Appareil)
	agent := tronquer(Masquer(env.Agent), 120)
	var lot []Evenement
	for _, brut := range offerts[:permis] {
		m, ok := brut.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := m["type"].(string)
		if !reType.MatchString(typ) || len(typ) > 40 {
			continue
		}
		delete(m, "type")
		msg, _ := m["message"].(string)
		delete(m, "message")
		d, _ := nettoyer(m, c.TexteMax, 0).(map[string]any)
		lot = append(lot, Evenement{Recu: now, Source: "client", Type: typ,
			Message: tronquer(Masquer(msg), c.TexteMax), Appareil: appareil, Agent: agent, Donnees: d})
	}
	if err := t.jour.ecrire(lot); err != nil {
		t.log.Warn("diagnostic : ecriture impossible", "erreur", err)
		erreur(w, http.StatusInternalServerError, "erreur_interne", "ecriture impossible")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]int{"recus": len(lot), "ignores": len(env.Evenement) - len(lot)})
}

// ---------------------------------------------------------------- debit

// limiteur : seau a jetons par session. Capacite = debit par minute. Les sessions
// suivies sont bornees (appareils_max) : la plus anciennement vue sort.
type limiteur struct {
	mu      sync.Mutex
	debit   float64 // jetons par seconde
	cap     float64
	max     int
	horloge func() time.Time
	seaux   map[string]*seau
}

type seau struct {
	jetons float64
	vu     time.Time
}

func nouveauLimiteur(parMinute, max int, h func() time.Time) *limiteur {
	return &limiteur{debit: float64(parMinute) / 60, cap: float64(parMinute), max: max, horloge: h, seaux: map[string]*seau{}}
}

// prendre retire jusqu'a n jetons ; renvoie le nombre accorde et, si zero, le
// delai avant le prochain jeton.
func (l *limiteur) prendre(cle string, n int) (int, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.horloge()
	s, ok := l.seaux[cle]
	if !ok {
		if len(l.seaux) >= l.max {
			l.evincer()
		}
		s = &seau{jetons: l.cap, vu: now}
		l.seaux[cle] = s
	}
	s.jetons = min(l.cap, s.jetons+now.Sub(s.vu).Seconds()*l.debit)
	s.vu = now
	accorde := min(n, int(s.jetons))
	s.jetons -= float64(accorde)
	if accorde == 0 && n > 0 {
		return 0, time.Duration((1 - s.jetons) / l.debit * float64(time.Second))
	}
	return accorde, 0
}

// evincer retire la session vue le plus anciennement (verrou tenu).
func (l *limiteur) evincer() {
	var plusVieux string
	var t time.Time
	for k, s := range l.seaux {
		if plusVieux == "" || s.vu.Before(t) {
			plusVieux, t = k, s.vu
		}
	}
	delete(l.seaux, plusVieux)
}
