package appli

import (
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
)

// interfaceStatique sert le build de l'interface (dossier configure). Un dossier
// n'est jamais liste ; tout chemin sans extension retombe sur index.html (routage
// cote client) ; un fichier absent AVEC extension reste un 404.
//
// En-tetes : CSP restrictive (aucun script ou style tiers ; les images peuvent venir
// des fournisseurs de logos ; les medias sont lus depuis ce service), jamais
// d'incrustation dans un cadre.
func interfaceStatique(dossier string, hsts bool) http.Handler {
	racine := os.DirFS(dossier)
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: http: https:; media-src 'self' blob:; "+
			"style-src 'self' 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
		if hsts {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			http.Error(w, "methode non permise", http.StatusMethodNotAllowed)
			return
		}
		p := path.Clean("/" + req.URL.Path)
		if strings.HasSuffix(req.URL.Path, "/") {
			p = path.Join(p, "index.html")
		}
		info, err := fs.Stat(racine, strings.TrimPrefix(p, "/"))
		switch {
		case err == nil && !info.IsDir():
			// fichier existant
		case path.Ext(p) == "":
			p = "/index.html" // route du client
		default:
			http.NotFound(w, req)
			return
		}
		if p == "/index.html" {
			h.Set("Cache-Control", "no-cache")
		}
		// ServeContent plutot que FileServer : ce dernier redirige /index.html vers « ./ »
		// (boucle avec notre repli) et expose les listes de dossier.
		f, err := racine.Open(strings.TrimPrefix(p, "/"))
		if err != nil {
			http.NotFound(w, req)
			return
		}
		defer f.Close()
		st, err := f.Stat()
		rs, ok := f.(io.ReadSeeker)
		if err != nil || st.IsDir() || !ok {
			http.NotFound(w, req)
			return
		}
		http.ServeContent(w, req, path.Base(p), st.ModTime(), rs)
	})
}
