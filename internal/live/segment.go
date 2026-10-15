package live

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"mytv/internal/domain"
	"mytv/internal/hls"
)

func (r *Relais) retryApres() string {
	return strconv.Itoa(int((r.opt.Bail.Reessai.Std() + time.Second - 1) / time.Second))
}

func (r *Relais) routeSegment(w http.ResponseWriter, req *http.Request) {
	chaine := req.PathValue("chaine")
	if !reChaine.MatchString(chaine) {
		http.Error(w, "chaine invalide", http.StatusBadRequest)
		return
	}
	cible, err := r.hls.Resoudre(chaine, req.PathValue("jeton"))
	switch {
	case errors.Is(err, hls.ErrInconnu):
		http.Error(w, "segment inconnu (playlist a recharger)", http.StatusNotFound)
		return
	case err != nil:
		http.Error(w, "jeton refuse", http.StatusForbidden)
		return
	}

	if err := r.hotes.verifier(req.Context(), cible.URL); err != nil {
		r.journal.Warn("segment refuse : hote non autorise", "chaine", chaine, "erreur", err)
		http.Error(w, "hote du segment non autorise", http.StatusForbidden)
		return
	}

	entetes := http.Header{}
	if v := req.Header.Get("Range"); v != "" {
		entetes.Set("Range", v)
	}
	// Une reponse partielle ne se range pas dans le cache, et un segment complet du
	// cache ne repond pas a une demande d'intervalle : on laisse alors le fournisseur faire.
	cachable := len(entetes) == 0
	if cachable {
		if corps, typ, ok := r.segments.lire(cible.Cle); ok {
			w.Header().Set("Content-Type", typ)
			w.Header().Set("Content-Length", strconv.Itoa(len(corps)))
			w.Header().Set("X-Relais", "cache")
			r.inst.Compter("live_segments_total", 1, "source", "cache")
			w.Write(corps)
			return
		}
	}

	bail, ok := r.bail(w, req, chaine)
	if !ok {
		return
	}

	debut := time.Now()
	var rep *http.Response
	var fin context.CancelFunc
	err = r.avecEssais(req.Context(), r.opt.Reseau.EssaisSegment, bail.Renouveler, func(ctx context.Context) error {
		var err error
		rep, fin, err = r.requete(ctx, cible.URL, entetes, r.opt.Reseau.DelaiSegment.Std())
		if err != nil {
			return err
		}
		if rep.StatusCode != http.StatusOK && rep.StatusCode != http.StatusPartialContent {
			code := rep.StatusCode
			rep.Body.Close()
			fin()
			return fmt.Errorf("%w : segment refuse (HTTP %d)", domain.ErrAmont, code)
		}
		return nil
	})
	if err != nil {
		if req.Context().Err() != nil {
			return
		}
		r.inst.Compter("live_segments_total", 1, "source", "erreur")
		r.journal.Warn("segment echec", "chaine", chaine, "ms", time.Since(debut).Milliseconds(), "erreur", err)
		if errors.Is(err, domain.ErrRefuseConnexions) {
			w.Header().Set("Retry-After", r.retryApres())
			http.Error(w, "connexion du fournisseur occupee", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, "segment indisponible", http.StatusBadGateway)
		return
	}
	defer fin()
	defer rep.Body.Close()

	typ := rep.Header.Get("Content-Type")
	// Une sous-playlist (playlist maitresse) doit etre reecrite, pas copiee (CA4 d'US-06).
	if strings.Contains(strings.ToLower(typ), "mpegurl") || strings.HasSuffix(strings.ToLower(strings.Split(cible.URL, "?")[0]), ".m3u8") {
		r.servirSousPlaylist(w, rep, chaine, cible.URL)
		return
	}

	if typ != "" {
		w.Header().Set("Content-Type", typ)
	}
	for _, h := range []string{"Content-Length", "Content-Range", "Accept-Ranges"} {
		if v := rep.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(rep.StatusCode)

	complet, clientParti := r.copier(w, req.Context(), rep, cachable && rep.StatusCode == http.StatusOK, cible.Cle, typ)
	if clientParti {
		// Fin de lecture : la place est rendue tout de suite, sans attendre le TTL
		// (sauf si la session a deja zappe vers une autre chaine).
		r.libererSi(sessionDe(req), chaine, bail)
		return
	}
	if complet {
		r.st.noterSegment(time.Since(debut))
		r.inst.Observer("live_duree_amont_secondes", time.Since(debut).Seconds(), "type", "segment")
		r.inst.Compter("live_segments_total", 1, "source", "amont")
		r.journal.Info("segment", "chaine", chaine, "ms", time.Since(debut).Milliseconds())
	}
}

// copier envoie le corps au client en flux, et en garde une copie si le segment
// est complet et de taille raisonnable (pour servir un nouvel essai du lecteur sans
// retourner chez le fournisseur). Il dit si la copie est complete et si le CLIENT
// est parti (ecriture en echec ou requete annulee).
func (r *Relais) copier(w http.ResponseWriter, ctx context.Context, rep *http.Response, garder bool, cle, typ string) (complet, clientParti bool) {
	limite := int64(r.opt.Live.CacheSegments.Octets / r.opt.Live.PartMaxSegment)
	if rep.ContentLength > limite {
		garder = false
	}
	var copie []byte
	buf := make([]byte, r.opt.Live.TailleTampon)
	flusher, _ := w.(http.Flusher)
	for {
		n, errLect := rep.Body.Read(buf)
		if n > 0 {
			if _, errEcr := w.Write(buf[:n]); errEcr != nil {
				return false, true // on n'enregistre pas un segment partiel
			}
			if garder {
				if int64(len(copie)+n) > limite {
					garder, copie = false, nil
				} else {
					copie = append(copie, buf[:n]...)
				}
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if errLect != nil {
			if errors.Is(errLect, io.EOF) {
				if garder && (rep.ContentLength < 0 || int64(len(copie)) == rep.ContentLength) {
					r.segments.ecrire(cle, copie, typ)
				}
				return true, false
			}
			if ctx.Err() != nil {
				return false, true
			}
			r.journal.Warn("segment interrompu", "erreur", errLect)
			return false, false
		}
	}
}

func (r *Relais) servirSousPlaylist(w http.ResponseWriter, rep *http.Response, chaine, cible string) {
	corps, err := io.ReadAll(io.LimitReader(rep.Body, int64(r.opt.Live.TaillePlaylistMax)))
	if err != nil {
		http.Error(w, "sous-playlist illisible", http.StatusBadGateway)
		return
	}
	base, _ := url.Parse(cible)
	if rep.Request != nil && rep.Request.URL != nil {
		base = rep.Request.URL
	}
	// Portee propre a la rendition : deux renditions peuvent nommer leurs segments pareil.
	portee := chaine + "/" + path.Base(base.Path)
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	io.WriteString(w, r.hls.Reecrire(r.prefixeSegments(chaine), portee, base, string(corps)))
}

func (r *Relais) routeEtat(w http.ResponseWriter, req *http.Request) {
	// Les statistiques disent qui lit quoi et quand le fournisseur flanche : un
	// administrateur seulement. Refus par defaut si EstAdmin n'est pas fourni.
	if r.opt.EstAdmin == nil || !r.opt.EstAdmin(req) {
		http.Error(w, "reserve aux administrateurs", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(r.Etat())
}
