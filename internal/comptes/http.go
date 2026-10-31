package comptes

import (
	"context"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"strings"
	"time"

	"mytv/internal/domain"
)

// IDSessionLocale est l'identifiant de session du compte technique. Il est stable
// (un seul lecteur sans cookie compte pour un seul bail, ce qui est voulu : le
// fournisseur n'autorise qu'une connexion).
const IDSessionLocale = "local"

// nomCookie : avec Secure, le prefixe __Host- interdit a un sous-domaine ou a une
// connexion en clair de poser ou d'ecraser le cookie.
func (s *Service) nomCookie() string {
	if s.cfg.CookieSecurise {
		return "__Host-mytv"
	}
	return "mytv"
}

// PoserCookie ecrit le cookie de session.
func (s *Service) PoserCookie(w http.ResponseWriter, jeton string, expire time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name: s.nomCookie(), Value: jeton, Path: "/", Expires: expire,
		HttpOnly: true, Secure: s.cfg.CookieSecurise, SameSite: http.SameSiteLaxMode,
	})
}

func (s *Service) effacerCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: s.nomCookie(), Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.cfg.CookieSecurise, SameSite: http.SameSiteLaxMode,
	})
}

// JetonDe lit le jeton du cookie (vide si absent).
func (s *Service) JetonDe(req *http.Request) string {
	c, err := req.Cookie(s.nomCookie())
	if err != nil {
		return ""
	}
	return c.Value
}

type cleSession struct{}

// SessionDe rend la session que Proteger a placee dans le contexte.
func SessionDe(ctx context.Context) (*domain.Session, bool) {
	s, ok := ctx.Value(cleSession{}).(*domain.Session)
	return s, ok && s != nil
}

func (s *Service) sessionCookie(req *http.Request) *domain.Session {
	if sess, ok := SessionDe(req.Context()); ok {
		return sess
	}
	jeton := s.JetonDe(req)
	if jeton == "" {
		return nil
	}
	sess, err := s.Session(req.Context(), jeton)
	if err != nil {
		return nil
	}
	return sess
}

// cleSessionImposee porte l'identifiant d'une session que la ROUTE a deja authentifiee
// autrement que par le cookie : la route de diffusion (US-40) a verifie un jeton signe
// qui designe la session. Elle ne vaut que pour la lecture (voir AuthentifierLecture).
type cleSessionImposee struct{}

// ImposerSession rend un contexte ou la requete est tenue pour venir de la session `id`.
// L'appelant a VERIFIE au prealable que la session existe encore (SessionParID) ; ce
// contexte ne donne jamais le role d'administrateur ni le droit d'ecrire.
func (s *Service) ImposerSession(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, cleSessionImposee{}, id)
}

func sessionImposee(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(cleSessionImposee{}).(string)
	return id, ok && id != ""
}

// Authentifier est l'authentificateur des routes qui exigent une VRAIE session
// (cookie). Refus par defaut : tout ce qui n'est pas une session valide est refuse.
func (s *Service) Authentifier(req *http.Request) (string, bool) {
	if sess := s.sessionCookie(req); sess != nil {
		return sess.ID, true
	}
	return "", false
}

// AuthentifierLecture est celui des routes de LECTURE (relais du direct, films).
// Dans l'ordre : la session imposee par la route (lien de diffusion signe, US-40 :
// la requete est servie COMME SI elle venait de cette session, meme bail, meme place
// chez le fournisseur), puis le cookie, puis le compte technique local pour les
// lecteurs du reseau local sans cookie. Session imposee et compte technique sont
// limites a GET/HEAD ; aucun des deux n'est jamais administrateur.
func (s *Service) AuthentifierLecture(req *http.Request) (string, bool) {
	lecture := req.Method == http.MethodGet || req.Method == http.MethodHead
	if id, ok := sessionImposee(req.Context()); ok && lecture {
		return id, true
	}
	if id, ok := s.Authentifier(req); ok {
		return id, true
	}
	if lecture && s.locale(req) {
		return IDSessionLocale, true
	}
	return "", false
}

// EstAdmin : la requete porte une session d'administrateur (jamais le compte local).
func (s *Service) EstAdmin(req *http.Request) bool {
	sess := s.sessionCookie(req)
	return sess != nil && sess.Admin
}

// Proteger exige une session par cookie ; la session est placee dans le contexte.
func (s *Service) Proteger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		sess := s.sessionCookie(req)
		if sess == nil {
			ecrireErreur(w, http.StatusUnauthorized, "non_authentifie", "authentification requise")
			return
		}
		next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), cleSession{}, sess)))
	})
}

// ProtegerAdmin exige en plus le role d'administrateur.
func (s *Service) ProtegerAdmin(next http.Handler) http.Handler {
	return s.Proteger(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if sess, _ := SessionDe(req.Context()); sess == nil || !sess.Admin {
			ecrireErreur(w, http.StatusForbidden, "interdit", "reserve aux administrateurs")
			return
		}
		next.ServeHTTP(w, req)
	}))
}

func ecrireJSON(w http.ResponseWriter, statut int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(statut)
	_ = json.NewEncoder(w).Encode(v)
}

func ecrireErreur(w http.ResponseWriter, statut int, code, message string) {
	ecrireJSON(w, statut, map[string]string{"code": code, "message": message})
}

type reponseSession struct {
	ID       string    `json:"id"`
	Nom      string    `json:"nom"`
	Admin    bool      `json:"admin"`
	ProfilID string    `json:"profil_id,omitempty"`
	Expire   time.Time `json:"expire"`
}

// Connexion est le gestionnaire POST de connexion : corps JSON {"nom","mot_de_passe"}.
// Le jeton va dans le cookie, jamais dans la reponse. corpsMax borne le corps.
func (s *Service) Connexion(corpsMax int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// Le type JSON obligatoire ferme la porte aux formulaires inter-sites.
		if mt, _, _ := mime.ParseMediaType(req.Header.Get("Content-Type")); mt != "application/json" {
			ecrireErreur(w, http.StatusUnsupportedMediaType, "type_incorrect", "Content-Type application/json requis")
			return
		}
		var in struct {
			Nom        string `json:"nom"`
			MotDePasse string `json:"mot_de_passe"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, req.Body, corpsMax))
		if err := dec.Decode(&in); err != nil {
			ecrireErreur(w, http.StatusBadRequest, "requete_invalide", "corps JSON invalide")
			return
		}
		jeton, sess, err := s.Connecter(req.Context(), in.Nom, in.MotDePasse, s.adresseClient(req))
		switch {
		case err == nil:
			s.PoserCookie(w, jeton, sess.Expire)
			ecrireJSON(w, http.StatusOK, reponseSession{sess.ID, sess.Nom, sess.Admin, sess.ProfilID, sess.Expire})
		case errors.Is(err, domain.ErrTropDeTentatives):
			w.Header().Set("Retry-After", "60")
			ecrireErreur(w, http.StatusTooManyRequests, "trop_de_tentatives", "trop de tentatives, reessayez plus tard")
		case errors.Is(err, domain.ErrIdentifiantsInvalides):
			ecrireErreur(w, http.StatusUnauthorized, "identifiants_invalides", "identifiants invalides")
		case req.Context().Err() != nil:
			// client parti
		default:
			s.journal.Error("connexion en erreur", "erreur", err)
			ecrireErreur(w, http.StatusInternalServerError, "erreur_interne", "erreur interne")
		}
	})
}

// Deconnexion revoque la session, libere ses baux (Terminer) et efface le cookie.
func (s *Service) Deconnexion() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if err := s.Deconnecter(req.Context(), strings.TrimSpace(s.JetonDe(req))); err != nil {
			s.journal.Error("deconnexion en erreur", "erreur", err)
			ecrireErreur(w, http.StatusInternalServerError, "erreur_interne", "erreur interne")
			return
		}
		s.effacerCookie(w)
		w.WriteHeader(http.StatusNoContent)
	})
}
