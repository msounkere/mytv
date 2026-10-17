package supervision

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

func ecrire(w http.ResponseWriter, statut int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(statut)
	_ = json.NewEncoder(w).Encode(v)
}

type erreurJSON struct {
	Code    string    `json:"code"`
	Quand   time.Time `json:"quand"`
	AgeSecs int64     `json:"age_secondes"`
}

// LigneFournisseur est un element de GET /admin/fournisseurs.
type LigneFournisseur struct {
	ID                string      `json:"id"`
	Etat              Etat        `json:"etat"`
	StatutCompte      string      `json:"statut_compte"`
	Expiration        *time.Time  `json:"expiration"`
	ExpireDans        *int64      `json:"expire_dans_secondes"`
	AlerteExpiration  bool        `json:"alerte_expiration"`
	ConnexionsMax     int         `json:"connexions_max"`
	ConnexionsActives int         `json:"connexions_actives"`
	InfoLueLe         *time.Time  `json:"info_lue_le"`
	DerniereSynchro   *time.Time  `json:"derniere_synchro_reussie"`
	AgeSynchro        *int64      `json:"age_synchro_secondes"`
	DerniereErreur    *erreurJSON `json:"derniere_erreur"`
	EchecsConsecutifs int         `json:"echecs_consecutifs"`
	// Occupation : qui tient la connexion unique (libre | mytv | ailleurs), depuis quand, et
	// la derniere fois qu'un autre appareil l'a prise en pleine lecture.
	Occupation    string     `json:"occupation"`
	Depuis        *time.Time `json:"depuis"`
	DernierePrise *time.Time `json:"derniere_prise"`
}

type reponseFournisseurs struct {
	Fournisseurs []LigneFournisseur `json:"fournisseurs"`
}

// Fournisseurs : GET /api/v1/admin/fournisseurs (administrateurs ; l'API gere l'acces).
func (m *Moniteur) Fournisseurs() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ecrire(w, http.StatusOK, reponseFournisseurs{Fournisseurs: m.Lignes(req.Context())})
	})
}

// Lignes rend l'etat de chaque fournisseur.
func (m *Moniteur) Lignes(ctx context.Context) []LigneFournisseur {
	now := m.horloge()
	out := make([]LigneFournisseur, 0, len(m.ordre))
	for _, id := range m.ordre {
		var derniere time.Time
		if m.o.DerniereSynchro != nil {
			derniere, _ = m.o.DerniereSynchro(ctx, id) // une erreur de lecture = « jamais » : la sante la signale
		}
		m.mu.Lock()
		f := m.fs[id]
		l := LigneFournisseur{
			ID: id, Etat: m.etatLocked(f, now), StatutCompte: f.info.Statut,
			ConnexionsMax: f.info.ConnexionsMax, ConnexionsActives: f.info.ConnexionsActives,
			EchecsConsecutifs: f.echecs,
		}
		if !f.info.Expiration.IsZero() {
			exp, sec := f.info.Expiration.UTC(), int64(f.info.Expiration.Sub(now).Seconds())
			l.Expiration, l.ExpireDans = &exp, &sec
			l.AlerteExpiration = f.info.Expiration.Sub(now) <= m.cfg.AlerteExpiration.Std()
		}
		if !f.infoLue.IsZero() {
			t := f.infoLue.UTC()
			l.InfoLueLe = &t
		}
		if f.erreurCode != "" {
			l.DerniereErreur = &erreurJSON{Code: f.erreurCode, Quand: f.erreurQuand.UTC(), AgeSecs: int64(now.Sub(f.erreurQuand).Seconds())}
		}
		m.mu.Unlock()
		l.Occupation = "libre"
		if m.o.Occupation != nil {
			l.Occupation, l.Depuis, l.DernierePrise = m.o.Occupation(id)
		}
		if !derniere.IsZero() {
			t, age := derniere.UTC(), int64(now.Sub(derniere).Seconds())
			l.DerniereSynchro, l.AgeSynchro = &t, &age
		}
		out = append(out, l)
	}
	return out
}

// Controle est un resultat de la sante approfondie.
type Controle struct {
	Nom    string `json:"nom"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

type reponseSante struct {
	Statut    string     `json:"statut"` // ok | degrade
	Controles []Controle `json:"controles"`
}

// Sante : GET /api/v1/admin/sante. 200 si tout va bien, 503 sinon (meme corps) pour
// qu'une sonde externe n'ait qu'un code a regarder.
func (m *Moniteur) Sante() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		cs := m.VerifierSante(req.Context())
		rep := reponseSante{Statut: "ok", Controles: cs}
		code := http.StatusOK
		for _, c := range cs {
			if !c.OK {
				rep.Statut, code = "degrade", http.StatusServiceUnavailable
			}
		}
		ecrire(w, code, rep)
	})
}

// VerifierSante controle base, fraicheur des synchronisations, stockage et fournisseurs.
// Le fournisseur n'est interroge que par user_info : aucun flux n'est ouvert.
func (m *Moniteur) VerifierSante(ctx context.Context) []Controle {
	var cs []Controle
	cs = append(cs, controle("base", func() (string, error) {
		if m.o.Base == nil {
			return "non verifiee", nil
		}
		return "", m.o.Base(ctx)
	}))
	cs = append(cs, controle("stockage", func() (string, error) {
		if m.o.Stockage == nil {
			return "non verifie", nil
		}
		return "", m.o.Stockage()
	}))
	now := m.horloge()
	// Les fournisseurs sont interroges en parallele : un fournisseur lent ne rallonge pas les autres.
	res := make([][2]Controle, len(m.ordre))
	var wg sync.WaitGroup
	for i, id := range m.ordre {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res[i][0] = m.controleSynchro(ctx, id, now)
			res[i][1] = m.controleJoignable(ctx, id)
		}()
	}
	wg.Wait()
	for _, r := range res {
		cs = append(cs, r[0], r[1])
	}
	return cs
}

func controle(nom string, f func() (string, error)) Controle {
	d, err := f()
	if err != nil {
		// Jamais le texte de l'erreur : un chemin ou une adresse pourrait s'y trouver.
		return Controle{Nom: nom, OK: false, Detail: "echec"}
	}
	return Controle{Nom: nom, OK: true, Detail: d}
}

func (m *Moniteur) controleSynchro(ctx context.Context, id string, now time.Time) Controle {
	nom := "synchro:" + id
	if m.o.DerniereSynchro == nil {
		return Controle{Nom: nom, OK: true, Detail: "non verifiee"}
	}
	derniere, err := m.o.DerniereSynchro(ctx, id)
	switch {
	case err != nil:
		return Controle{Nom: nom, Detail: "lecture impossible"}
	case derniere.IsZero():
		return Controle{Nom: nom, Detail: "jamais synchronise"}
	case now.Sub(derniere) >= 2*m.fs[id].src.AgeMax:
		return Controle{Nom: nom, Detail: "derniere synchronisation trop ancienne"}
	}
	return Controle{Nom: nom, OK: true}
}

func (m *Moniteur) controleJoignable(ctx context.Context, id string) Controle {
	nom := "fournisseur:" + id
	f := m.fs[id]
	if f.src.Info == nil {
		return Controle{Nom: nom, OK: true, Detail: "non verifie"}
	}
	// La lecture met aussi l'etat a jour : un compte expire est vu ici.
	if err := m.rafraichir(ctx, f); err != nil {
		return Controle{Nom: nom, Detail: Classe(err).Code()}
	}
	m.mu.Lock()
	invalide := !statutActif(f.info.Statut) || (!f.info.Expiration.IsZero() && !f.info.Expiration.After(m.horloge()))
	m.mu.Unlock()
	if invalide {
		return Controle{Nom: nom, Detail: EtatCompteInvalide.Code()}
	}
	return Controle{Nom: nom, OK: true}
}
