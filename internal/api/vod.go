package api

import (
	"errors"
	"net/http"
	"strings"

	"mytv/internal/domain"
	"mytv/internal/occupation"
)

func (a *API) declarerVOD() {
	a.routeLecture("GET", "/vod/{id}", a.lire)
}

// suiveur note si la reponse a commence : apres cela, une erreur ne peut plus
// devenir un JSON propre, on ne peut que journaliser.
type suiveur struct {
	http.ResponseWriter
	ecrit bool
}

func (s *suiveur) WriteHeader(c int)           { s.ecrit = true; s.ResponseWriter.WriteHeader(c) }
func (s *suiveur) Write(b []byte) (int, error) { s.ecrit = true; return s.ResponseWriter.Write(b) }
func (s *suiveur) Unwrap() http.ResponseWriter { return s.ResponseWriter }
func (s *suiveur) Flush() {
	s.ecrit = true
	_ = http.NewResponseController(s.ResponseWriter).Flush()
}

func (a *API) lire(w http.ResponseWriter, req *http.Request, session string) {
	if a.opt.Lecture == nil {
		erreur(w, http.StatusNotImplemented, "non_disponible", "lecture des films non activee")
		return
	}
	id, ok := idDuChemin(w, req)
	if !ok {
		return
	}
	sw := &suiveur{ResponseWriter: w}
	err := a.opt.Lecture.Servir(sw, req, session, id)
	if err == nil || contexteAnnule(req.Context()) {
		return
	}
	if sw.ecrit {
		// Rare : une lecture entamee qui echoue est abandonnee par Servir lui-meme
		// (panic(http.ErrAbortHandler), relancee par recuperer) pour que le client voie une
		// erreur reseau et non une fin de film. Ici ne reste que le client parti de lui-meme.
		a.log.Warn("lecture interrompue", "erreur", err)
		return
	}
	if errors.Is(err, domain.ErrNonLisible) {
		// Le code et le message precis (HEVC...) viennent de l'erreur quand elle les porte :
		// l'interface peut alors proposer autre chose qu'un echec generique.
		code, message := "format_non_lisible", "format non lisible dans un navigateur"
		var f interface {
			CodeAPI() string
			MessageAPI() string
		}
		if errors.As(err, &f) {
			code, message = f.CodeAPI(), f.MessageAPI()
		}
		erreur(w, http.StatusUnsupportedMediaType, code, message)
		return
	}
	if occupation.EstOccupation(err) {
		// Le fournisseur (ou une autre session de mytv) tient la connexion : le code dit
		// laquelle, et Retry-After croit a chaque refus consecutif de ce film.
		fournisseur, _, _ := strings.Cut(string(id), ":")
		a.opt.Occupation.Repondre(w, occupation.Demande{Fournisseur: fournisseur, Session: session, Ressource: string(id)}, err)
		return
	}
	a.erreurAmont(w, err, "contenu")
}
