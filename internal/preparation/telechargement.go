package preparation

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"mytv/internal/domain"
	"mytv/internal/remux"
)

var (
	// errPause : le telechargement a ete coupe pour laisser la place a une lecture (ou le
	// fournisseur l'a refusee) ; le `.partiel` est conserve et sera repris.
	errPause = errors.New("preparation : telechargement mis en pause")
	// errBudget : pas de place sur le disque pour ce fichier ; il reste en attente.
	errBudget = errors.New("preparation : budget disque insuffisant")
	// errReponse : reponse du fournisseur inexploitable (200 a la place de 206, Content-Range
	// incoherent, longueur inconnue).
	errReponse = errors.New("preparation : reponse du fournisseur inexploitable")
)

const (
	// reprisesMax : nouvelles requetes Range apres une coupure du fournisseur, par fichier
	// et par cycle ; au-dela, le fichier reste en attente pour le cycle suivant.
	reprisesMax = 8
	// essaisTransitoires : nouveaux essais sur une erreur amont a l'ouverture (5xx, reseau).
	essaisTransitoires = 3
)

// reculs : attente avant la nouvelle tentative automatique d'un echec transitoire, selon le
// nombre d'echecs consecutifs ; le dernier se repete sans fin tant que l'element est dans
// la liste (US-44 CA2).
var reculs = []time.Duration{15 * time.Minute, 30 * time.Minute, time.Hour, 2 * time.Hour, 4 * time.Hour}

// telecharger amene le fichier de `v` a son terme : ouverture (Range depuis les octets
// deja ecrits), verification de la reponse, copie dans `.partiel`, reprise apres coupure,
// taille verifiee, renommage. Une seule connexion a la fois (le moteur appelle en serie).
func (m *Moteur) telecharger(ctx context.Context, e *entree, v voulu) (err error) {
	f := m.dep.Fournisseurs[v.fid]
	src := m.srcs[v.fid]
	if f == nil || src == nil {
		return m.echec(e, domain.ErrNonTrouve)
	}
	url := src.URLFilm(v.numero, v.extension)
	if v.episode {
		url = src.URLEpisode(v.numero, v.extension)
	}
	// Taille deja connue (cycle precedent) : la place se regle sans ouvrir de connexion.
	if e.Total > 0 {
		if e.Total > m.r.Budget {
			return m.echec(e, fmt.Errorf("fichier de %d octets plus grand que le budget", e.Total))
		}
		if !m.faireDeLaPlace(e, e.Total) {
			return m.attente(e, "budget disque insuffisant", errBudget)
		}
	}
	ctx, annuler := context.WithCancel(ctx)
	defer annuler()
	c := &telechargement{fid: v.fid, contenu: e.Contenu, annuler: annuler, fini: make(chan struct{})}
	m.mu.Lock()
	m.courant = c
	e.Etat, e.Cause, e.Reessai, e.budgetPlein = domain.PreparationEnCours, "", time.Time{}, false
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.courant = nil
		e.debitKbps = 0
		m.sauverIndexLocked()
		m.mu.Unlock()
		close(c.fini)
	}()

	// Ce qui est deja sur le disque (un .partiel d'un cycle ou d'un demarrage precedent).
	chemin := filepath.Join(m.r.Dossier, e.partiel())
	var deja int64
	if st, err := os.Stat(chemin); err == nil {
		deja = st.Size()
	}
	m.mu.Lock()
	e.Octets = deja
	m.mu.Unlock()
	debut := m.now()
	depart := deja

	buf := make([]byte, m.r.Tampon)
	reprises := 0
	for {
		corps, total, err := m.ouvrir(ctx, f, url, e.Octets)
		if err != nil {
			return m.erreurOuverture(e, err)
		}
		if e.Total > 0 && total != e.Total {
			// Le fichier a change chez le fournisseur : on repart de zero.
			corps.Close()
			m.log.Warn("preparation : taille changee chez le fournisseur, reprise a zero", "contenu", string(e.Contenu), "avant", e.Total, "apres", total)
			os.Remove(chemin)
			m.mu.Lock()
			e.Octets, e.Total = 0, total
			m.mu.Unlock()
			depart = 0
			continue
		}
		if e.Total == 0 {
			if total > m.r.Budget {
				corps.Close()
				return m.echec(e, fmt.Errorf("fichier de %d octets plus grand que le budget", total))
			}
			m.mu.Lock()
			e.Total = total
			m.mu.Unlock()
			if !m.faireDeLaPlace(e, total) {
				corps.Close()
				return m.attente(e, "budget disque insuffisant", errBudget)
			}
		}
		// Le .partiel n'est ouvert (cree) qu'une fois la reponse verifiee et la place faite :
		// un fichier en attente n'occupe rien sur le disque.
		fichier, err := os.OpenFile(chemin, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			corps.Close()
			return m.echec(e, err)
		}
		fin, err := m.copier(ctx, e, fichier, corps, buf)
		corps.Close()
		if fin {
			err := m.terminer(e, fichier, debut, depart)
			fichier.Close()
			return err
		}
		fichier.Close()
		switch {
		case ctx.Err() != nil:
			return m.attente(e, "", errPause)
		case reprises >= reprisesMax:
			return m.echecTransitoire(e, fmt.Errorf("coupures repetees du fournisseur : %w", err))
		}
		reprises++
		m.inst.Compter("preparation_reprises_total", 1)
		m.log.Info("preparation : coupure du fournisseur, reprise", "contenu", string(e.Contenu), "octets", e.Octets, "erreur", assainir(err))
		select {
		case <-time.After(time.Duration(reprises) * 500 * time.Millisecond):
		case <-ctx.Done():
			return m.attente(e, "", errPause)
		}
	}
}

// ouvrir fait la requete amont a partir de `depuis` et verifie la reponse : 200 et
// Content-Length quand on part de zero, sinon 206 et un Content-Range qui commence
// exactement a `depuis` ; corps non encode ; taille totale connue. Les erreurs amont
// transitoires sont retentees quelques fois.
func (m *Moteur) ouvrir(ctx context.Context, f domain.Fournisseur, url string, depuis int64) (io.ReadCloser, int64, error) {
	h := http.Header{"Accept-Encoding": {"identity"}}
	if depuis > 0 {
		h.Set("Range", "bytes="+strconv.FormatInt(depuis, 10)+"-")
	}
	var rep *http.Response
	var err error
	for essai := 0; ; essai++ {
		rep, err = f.Requete(ctx, url, h)
		if err == nil || !errors.Is(err, domain.ErrAmont) || essai >= essaisTransitoires || ctx.Err() != nil {
			break
		}
		select {
		case <-time.After(time.Duration(essai+1) * 250 * time.Millisecond):
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		}
	}
	if err != nil {
		return nil, 0, err
	}
	refus := func(raison string) (io.ReadCloser, int64, error) {
		rep.Body.Close()
		return nil, 0, fmt.Errorf("%w : %s", errReponse, raison)
	}
	if ce := rep.Header.Get("Content-Encoding"); ce != "" && ce != "identity" {
		return refus("corps encode")
	}
	if depuis == 0 {
		if rep.StatusCode != http.StatusOK {
			return refus("statut " + strconv.Itoa(rep.StatusCode) + " pour une lecture complete")
		}
		if rep.ContentLength <= 0 {
			return refus("longueur inconnue")
		}
		return rep.Body, rep.ContentLength, nil
	}
	if rep.StatusCode != http.StatusPartialContent {
		return refus("statut " + strconv.Itoa(rep.StatusCode) + " a la place de 206")
	}
	d, fin, total, ok := analyserContentRange(rep.Header.Get("Content-Range"))
	if !ok || d != depuis || total <= fin {
		return refus("Content-Range incoherent avec la reprise")
	}
	if rep.ContentLength >= 0 && rep.ContentLength != fin-d+1 {
		return refus("Content-Length different de Content-Range")
	}
	return rep.Body, total, nil
}

// analyserContentRange lit « bytes a-b/total » ; ok = faux si la forme ou le total manque.
func analyserContentRange(h string) (debut, fin, total int64, ok bool) {
	spec, ok := strings.CutPrefix(strings.TrimSpace(h), "bytes ")
	if !ok {
		return 0, 0, 0, false
	}
	bornes, tot, ok := strings.Cut(spec, "/")
	if !ok {
		return 0, 0, 0, false
	}
	a, b, ok := strings.Cut(bornes, "-")
	if !ok {
		return 0, 0, 0, false
	}
	var e1, e2, e3 error
	debut, e1 = strconv.ParseInt(strings.TrimSpace(a), 10, 64)
	fin, e2 = strconv.ParseInt(strings.TrimSpace(b), 10, 64)
	total, e3 = strconv.ParseInt(strings.TrimSpace(tot), 10, 64)
	if e1 != nil || e2 != nil || e3 != nil || debut < 0 || fin < debut || total <= fin {
		return 0, 0, 0, false
	}
	return debut, fin, total, true
}

// copier ecrit le corps dans le fichier jusqu'a la taille totale. fin = vrai si le fichier
// est complet ; sinon err dit pourquoi (coupure, annulation).
func (m *Moteur) copier(ctx context.Context, e *entree, fichier *os.File, corps io.Reader, buf []byte) (fin bool, err error) {
	fenetreDebut, fenetreOctets := m.now(), int64(0)
	for {
		m.mu.Lock()
		reste := e.Total - e.Octets
		m.mu.Unlock()
		if reste <= 0 {
			return true, nil
		}
		n, rerr := corps.Read(buf[:min(int64(len(buf)), reste)])
		if n > 0 {
			if _, werr := fichier.Write(buf[:n]); werr != nil {
				return false, werr
			}
			m.inst.Compter("preparation_octets_total", float64(n))
			fenetreOctets += int64(n)
			m.mu.Lock()
			e.Octets += int64(n)
			if d := m.now().Sub(fenetreDebut); d >= time.Second {
				e.debitKbps = int(float64(fenetreOctets) * 8 / 1000 / d.Seconds())
				fenetreDebut, fenetreOctets = m.now(), 0
			}
			complet := e.Octets >= e.Total
			m.mu.Unlock()
			if complet {
				return true, nil
			}
		}
		if rerr != nil {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			if errors.Is(rerr, io.EOF) {
				rerr = io.ErrUnexpectedEOF
			}
			return false, rerr
		}
	}
}

// terminer verifie la taille, renomme le `.partiel` et publie journal et mesures.
func (m *Moteur) terminer(e *entree, fichier *os.File, debut time.Time, depart int64) error {
	if err := fichier.Sync(); err != nil {
		return m.echec(e, err)
	}
	st, err := fichier.Stat()
	if err != nil {
		return m.echec(e, err)
	}
	if st.Size() != e.Total {
		os.Remove(filepath.Join(m.r.Dossier, e.partiel()))
		m.mu.Lock()
		e.Octets = 0
		m.mu.Unlock()
		return m.echec(e, fmt.Errorf("%w : %d octets ecrits pour %d annonces", errReponse, st.Size(), e.Total))
	}
	if err := os.Rename(filepath.Join(m.r.Dossier, e.partiel()), filepath.Join(m.r.Dossier, e.Nom)); err != nil {
		return m.echec(e, err)
	}
	duree := m.now().Sub(debut)
	debit := 0
	if duree > 0 {
		debit = int(float64(e.Total-depart) * 8 / 1000 / duree.Seconds())
	}
	m.mu.Lock()
	e.Etat, e.Cause, e.Octets, e.PretLe = domain.PreparationPrete, "", e.Total, m.now()
	e.Reessai, e.Echecs = time.Time{}, 0
	m.sauverIndexLocked()
	m.mu.Unlock()
	m.inst.Compter("preparation_fichiers_total", 1, "etat", "pret")
	m.inst.Evenement("preparation:pret", "fichier prepare : "+string(e.Contenu))
	m.log.Info("preparation : fichier pret", "contenu", string(e.Contenu), "octets", e.Total, "duree_ms", duree.Milliseconds(), "debit_kbps", debit)
	return nil
}

// erreurOuverture classe l'erreur d'une ouverture : un contexte annule ou un refus de
// place (407) mettent en pause ; un 404/410, des identifiants refuses, un contenu disparu
// du catalogue ou une reponse inexploitable sont un echec PERMANENT ; le reste (coupure,
// delai, 5xx, 458 adresse refusee) est un echec TRANSITOIRE, retente a `reessai`.
func (m *Moteur) erreurOuverture(e *entree, err error) error {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return m.attente(e, "", errPause)
	case errors.Is(err, domain.ErrRefuseConnexions):
		// Quelqu'un tient la place (autre appareil) : comme une lecture, on repasse plus tard.
		m.mu.Lock()
		m.finLecture[m.courant.fid] = m.now()
		m.mu.Unlock()
		m.inst.Compter("preparation_refus_total", 1)
		return m.attente(e, "place occupee chez le fournisseur", errPause)
	case errors.Is(err, domain.ErrIntrouvable), errors.Is(err, domain.ErrIdentifiants), errors.Is(err, domain.ErrNonTrouve), errors.Is(err, errReponse):
		return m.echec(e, err)
	}
	return m.echecTransitoire(e, err)
}

// attente remet l'entree en attente (fichier partiel conserve) avec une cause lisible.
func (m *Moteur) attente(e *entree, cause string, err error) error {
	m.mu.Lock()
	e.Etat, e.Cause, e.budgetPlein = domain.PreparationAttente, cause, errors.Is(err, errBudget)
	m.mu.Unlock()
	if errors.Is(err, errPause) {
		m.inst.Compter("preparation_pauses_total", 1)
		m.log.Info("preparation : telechargement en pause", "contenu", string(e.Contenu), "octets", e.Octets, "total", e.Total)
	}
	return err
}

// echec abandonne le fichier (cause assainie, partiel efface) : echec permanent, sans
// nouvelle tentative automatique (POST .../reessayer la demande).
func (m *Moteur) echec(e *entree, err error) error {
	m.mu.Lock()
	m.effacerLocked(e)
	e.Etat, e.Cause, e.Reessai, e.Echecs = domain.PreparationEchec, assainir(err), time.Time{}, 0
	m.mu.Unlock()
	m.inst.Compter("preparation_fichiers_total", 1, "etat", "echec")
	m.log.Warn("preparation : fichier abandonne", "contenu", string(e.Contenu), "cause", assainir(err))
	return fmt.Errorf("preparation : %w", err)
}

// echecTransitoire : la cause est reseau (coupure, delai, 5xx, 458) : l'etat est `echec`,
// le partiel est conserve, et une nouvelle tentative est programmee a `reessai` avec un
// recul croissant (15 min, 30 min, 1 h, 2 h, 4 h), sans fin tant que l'element est demande.
func (m *Moteur) echecTransitoire(e *entree, err error) error {
	m.mu.Lock()
	recul := reculs[min(e.Echecs, len(reculs)-1)]
	e.Echecs++
	e.Etat, e.Cause, e.Reessai = domain.PreparationEchec, assainir(err), m.now().Add(recul)
	reessai := e.Reessai
	m.mu.Unlock()
	m.inst.Compter("preparation_fichiers_total", 1, "etat", "echec_transitoire")
	m.log.Warn("preparation : echec transitoire, nouvelle tentative programmee", "contenu", string(e.Contenu), "cause", assainir(err),
		"reessai", reessai.UTC().Format(time.RFC3339), "recul", recul.String())
	return fmt.Errorf("preparation : %w", err)
}

// assainir retire d'un message d'erreur ce qu'un transport pourrait charrier (URL, jeton).
func assainir(err error) string {
	if err == nil {
		return ""
	}
	s := remux.Assainir(err.Error())
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
