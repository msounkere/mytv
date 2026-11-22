package api

// /xmltv.php (lot 11) : le guide des chaines envoyees a la box, au format XMLTV.
//
// La box rapproche un programme de sa chaine par `epg_channel_id` (get_live_streams) : le guide
// utilise exactement le meme identifiant (celui du fournisseur, ou « mytv.<numero> » s'il n'y en a
// pas), et les programmes viennent du guide de mytv tel qu'il est rapproche de CHAQUE chaine. Les
// variantes d'une chaine partagent leur identifiant : ses programmes ne sont ecrits qu'une fois.
// Le filtre des langues et pays (Reglages > Box IPTV) s'applique : pas de guide pour 22 000 chaines.

import (
	"compress/gzip"
	"encoding/xml"
	"io"
	"net/http"
	"strings"
	"time"

	"mytv/internal/config"
	"mytv/internal/domain"
)

const (
	xmltvPasse  = 24 * time.Hour     // programmes passes envoyes (replay a venir)
	xmltvFutur  = 3 * 24 * time.Hour // programmes a venir
	formatXMLTV = "20060102150405 -0700"
)

// idGuideXtream : identifiant de guide d'une chaine, le meme dans get_live_streams et XMLTV.
func idGuideXtream(c domain.Chaine) string {
	if c.EpgID != "" {
		return c.EpgID
	}
	return "mytv." + c.Numero
}

func ecrireTexteXML(w io.Writer, s string) {
	_ = xml.EscapeText(w, []byte(s))
}

func (a *API) xtreamXMLTV(w http.ResponseWriter, req *http.Request, x *etatXtream, cfg config.Xtream) {
	q := req.URL.Query()
	if _, err := a.sessionXtream(req.Context(), x, q.Get("username"), q.Get("password"), adresseXtream(req)); err != nil {
		refusXtream(w, err)
		return
	}
	if a.opt.Export == nil || a.opt.EPG == nil {
		http.Error(w, "guide non active", http.StatusNotImplemented)
		return
	}
	ctx := req.Context()
	filtre := a.filtreXtream(ctx)
	var retenues map[string]bool
	if filtre != nil {
		var err error
		if retenues, err = a.categoriesRetenues(ctx, domain.ExportLive, filtre); err != nil {
			http.Error(w, "catalogue indisponible", http.StatusBadGateway)
			return
		}
	}
	// Une chaine par identifiant de guide (la premiere : les variantes ont le meme guide).
	type canal struct {
		id        string
		chaine    domain.ContenuID
		nom, logo string
	}
	var canaux []canal
	vus := map[string]bool{}
	ex, err := a.opt.Export.OuvrirExport(ctx, domain.ExportLive, a.opt.FournisseurLive)
	if err != nil {
		http.Error(w, "catalogue indisponible", http.StatusBadGateway)
		return
	}
	fx := a.fournisseurXtream()
	err = ex.Lire(func(v any) error {
		c, ok := v.(domain.Chaine)
		if !ok || c.FournisseurID != fx || !filtre.garde(c.Nom, c.CategorieID, retenues) {
			return nil
		}
		id := idGuideXtream(c)
		if !vus[id] {
			vus[id] = true
			canaux = append(canaux, canal{id: id, chaine: c.ID, nom: c.Nom, logo: c.Logo})
		}
		return nil
	})
	ex.Fermer()
	if err != nil {
		http.Error(w, "catalogue indisponible", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	var sortie io.Writer = w
	if strings.Contains(req.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Add("Vary", "Accept-Encoding")
		gz := gzip.NewWriter(w)
		defer gz.Close()
		sortie = gz
	}
	a.inst.Compter("xtream_requetes_total", 1, "action", "xmltv")
	_, _ = io.WriteString(sortie, `<?xml version="1.0" encoding="UTF-8"?>`+"\n"+`<tv generator-info-name="mytv">`+"\n")
	for _, c := range canaux {
		_, _ = io.WriteString(sortie, `<channel id="`)
		ecrireTexteXML(sortie, c.id)
		_, _ = io.WriteString(sortie, `"><display-name>`)
		ecrireTexteXML(sortie, c.nom)
		_, _ = io.WriteString(sortie, `</display-name>`)
		if c.logo != "" {
			_, _ = io.WriteString(sortie, `<icon src="`)
			ecrireTexteXML(sortie, c.logo)
			_, _ = io.WriteString(sortie, `"/>`)
		}
		_, _ = io.WriteString(sortie, "</channel>\n")
	}
	maintenant := a.maintenant().UTC()
	const lot = 200
	for i := 0; i < len(canaux) && ctx.Err() == nil; i += lot {
		part := canaux[i:min(i+lot, len(canaux))]
		ids := make([]domain.ContenuID, len(part))
		for j, c := range part {
			ids[j] = c.chaine
		}
		g, err := a.opt.EPG.Grille(ctx, ids, maintenant.Add(-xmltvPasse), maintenant.Add(xmltvFutur))
		if err != nil {
			a.log.Warn("xtream : guide XMLTV incomplet", "erreur", err)
			break
		}
		for _, c := range part {
			for _, p := range g[c.chaine] {
				_, _ = io.WriteString(sortie, `<programme start="`+p.Debut.UTC().Format(formatXMLTV)+`" stop="`+p.Fin.UTC().Format(formatXMLTV)+`" channel="`)
				ecrireTexteXML(sortie, c.id)
				_, _ = io.WriteString(sortie, `"><title>`)
				ecrireTexteXML(sortie, p.Titre)
				_, _ = io.WriteString(sortie, `</title>`)
				if p.SousTitre != "" {
					_, _ = io.WriteString(sortie, `<sub-title>`)
					ecrireTexteXML(sortie, p.SousTitre)
					_, _ = io.WriteString(sortie, `</sub-title>`)
				}
				if p.Description != "" {
					_, _ = io.WriteString(sortie, `<desc>`)
					ecrireTexteXML(sortie, p.Description)
					_, _ = io.WriteString(sortie, `</desc>`)
				}
				if p.Categorie != "" {
					_, _ = io.WriteString(sortie, `<category>`)
					ecrireTexteXML(sortie, p.Categorie)
					_, _ = io.WriteString(sortie, `</category>`)
				}
				_, _ = io.WriteString(sortie, "</programme>\n")
			}
		}
	}
	_, _ = io.WriteString(sortie, "</tv>\n")
}
