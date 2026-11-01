package vod

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"time"

	"mytv/internal/domain"
)

// Lecture depuis le disque (US-42 CA5) : quand la preparation d'avance tient un fichier
// COMPLET pour le contenu demande, il est servi comme source locale, par les memes
// chemins que le fournisseur (sonde, decision de mode, direct / remux / audio converti /
// eco), SANS fournisseur et SANS bail. Un fichier partiel ou absent laisse le chemin habituel.

// Prepares est ce que le service attend du moteur de preparation.
type Prepares interface {
	// Pret rend le chemin d'un fichier complet et une fonction a appeler en fin de lecture
	// (tant qu'elle n'est pas appelee, le fichier n'est pas efface). ok = faux sinon.
	Pret(id domain.ContenuID) (chemin string, liberer func(), ok bool)
}

// enteteSource est pose sur toute reponse servie depuis un fichier prepare.
const enteteSource = "X-Mytv-Source"

// sourceLocale ouvre le fichier prepare a l'octet `depuis` ; le lecteur rendu connait la
// taille totale (domain.AvecTaille) : la source locale de ffmpeg sert les plages, et
// ffprobe lit l'index de fin de fichier sans tout parcourir.
func sourceLocale(chemin string) domain.SourceOctets {
	return func(ctx context.Context, depuis int64) (io.ReadCloser, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		f, err := os.Open(chemin)
		if err != nil {
			return nil, err
		}
		st, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, err
		}
		if depuis < 0 || depuis >= st.Size() {
			f.Close()
			return nil, domain.ErrPlageHorsFichier{Total: st.Size()}
		}
		if _, err := f.Seek(depuis, io.SeekStart); err != nil {
			f.Close()
			return nil, err
		}
		return &fichierLocal{File: f, taille: st.Size()}, nil
	}
}

type fichierLocal struct {
	*os.File
	taille int64
}

func (f *fichierLocal) Taille() int64 { return f.taille }

// directLocal sert le fichier prepare tel quel : Range exact, 206 et Content-Range par
// http.ServeContent (seek dans le fichier, pas de connexion), HEAD sans corps.
func (s *Service) directLocal(ctx context.Context, w http.ResponseWriter, req *http.Request, m *media, chemin string, debut time.Time, info infosLecture) error {
	f, err := os.Open(chemin)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	// Une plage hors du fichier rend l'erreur du contrat (416 + Content-Range: */total)
	// avant tout en-tete, comme le relais du fournisseur.
	if p := analyserPlage(req.Header.Get("Range")); p.presente && p.debut >= st.Size() {
		return domain.ErrPlageHorsFichier{Total: st.Size()}
	}
	h := w.Header()
	h.Set("Content-Type", typeContenu(m.extension, info.brut))
	h.Set("Cache-Control", "private, no-store")
	info.poser(h)
	s.inst.Observer("vod_premier_octet_secondes", time.Since(debut).Seconds(), "mode", string(domain.ModeDirect))
	http.ServeContent(w, req, "", time.Time{}, f)
	if err := ctx.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
