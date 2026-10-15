package api

import (
	"net/http"
	"net/url"
	"slices"
	"strings"

	"mytv/internal/config"
)

// enteteSecurite pose les en-tetes de securite par defaut sur toute reponse de l'API.
// Une API JSON n'a besoin de charger aucune ressource : CSP « default-src 'none' ».
func enteteSecurite(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		next.ServeHTTP(w, req)
	})
}

// cors ferme par defaut : sans api.origines_cors, aucun en-tete CORS n'est jamais
// envoye. Il porte aussi la defense anti-CSRF : une requete d'ecriture venant d'une
// autre origine que la notre (ou qu'une origine listee) est refusee, meme si le
// navigateur y a joint le cookie.
func (a *API) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		origine := req.Header.Get("Origin")
		if origine == "" {
			next.ServeHTTP(w, req)
			return
		}
		w.Header().Add("Vary", "Origin")
		autorisee := slices.Contains(a.opt.Config.OriginesCORS, origine)
		if !autorisee && !memeOrigine(origine, req.Host) {
			if !estSur(req.Method) || req.Method == http.MethodOptions {
				erreur(w, http.StatusForbidden, "origine_refusee", "origine non autorisee")
				return
			}
			next.ServeHTTP(w, req) // lecture : sans en-tete CORS, le navigateur bloque la lecture
			return
		}
		if autorisee {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origine)
			h.Set("Access-Control-Allow-Credentials", "true")
			h.Set("Access-Control-Expose-Headers", "ETag, Retry-After, X-Elements")
			if req.Method == http.MethodOptions && req.Header.Get("Access-Control-Request-Method") != "" {
				h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE")
				h.Set("Access-Control-Allow-Headers", "Content-Type, If-None-Match")
				h.Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, req)
	})
}

func memeOrigine(origine, hote string) bool {
	u, err := url.Parse(origine)
	return err == nil && u.Host != "" && strings.EqualFold(u.Host, hote)
}

// limites borne la taille des corps de requete (config api.corps_max). Les delais
// de lecture et d'ecriture sont ceux du serveur (voir Serveur).
func (a *API) limites(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.ContentLength > a.opt.Config.CorpsMax {
			erreur(w, http.StatusRequestEntityTooLarge, "corps_trop_gros", "corps de requete trop gros")
			return
		}
		if req.Body != nil {
			req.Body = http.MaxBytesReader(w, req.Body, a.opt.Config.CorpsMax)
		}
		next.ServeHTTP(w, req)
	})
}

// Serveur construit le serveur HTTP avec les delais de la configuration. Les flux
// longs retirent eux-memes leur delai d'ecriture (sansDelaiEcriture).
func Serveur(ecoute string, h http.Handler, c config.API) *http.Server {
	return &http.Server{
		Addr:              ecoute,
		Handler:           h,
		ReadHeaderTimeout: c.DelaiLecture.Std(),
		ReadTimeout:       c.DelaiLecture.Std(),
		WriteTimeout:      c.DelaiEcriture.Std(),
		IdleTimeout:       c.DelaiEcriture.Std(),
		MaxHeaderBytes:    int(c.CorpsMax),
	}
}
