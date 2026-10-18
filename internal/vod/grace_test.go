package vod

import (
	"bytes"
	"io"
	"net/http"
	"testing"
	"time"

	"mytv/internal/testutil/fauxiptv"
)

// Grace d'annulation (vod.grace_annulation). Un navigateur en lecture directe emet des
// plages concurrentes legitimes sur le MEME film (index en fin de fichier, position posee a
// `loadedmetadata`, prechargement). Les annuler des l'arrivee de la suivante donnait des
// rafales de 499, des reessais et un risque de MEDIA_ERR_NETWORK.

type resultatGet struct {
	rep   *http.Response
	corps []byte
	err   error
}

// getEnFond : comme b.get, mais hors de la goroutine du test (aucun t.Fatal ici) ; une
// erreur de transport est rendue comme telle.
func (b *banc) getEnFond(id, session, plage string) <-chan resultatGet {
	ch := make(chan resultatGet, 1)
	go func() {
		req, _ := http.NewRequest("GET", b.http.URL+"/?id="+id, nil)
		req.Header.Set("X-Session", session)
		req.Header.Set("Range", plage)
		rep, err := http.DefaultClient.Do(req)
		if err != nil {
			ch <- resultatGet{&http.Response{}, nil, err}
			return
		}
		defer rep.Body.Close()
		corps, err := io.ReadAll(rep.Body)
		ch <- resultatGet{rep, corps, err}
	}()
	return ch
}

// Deux plages successives du meme film : la premiere, encore en cours (le fournisseur met
// 150 ms a repondre), finit d'elle-meme pendant la grace ; aucune des deux n'est annulee.
func TestGrace_LesPlagesConcurrentesDeLaMemeRessourceNeSontPasAnnulees(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, func(r *Reglages) { r.GraceAnnulation = time.Second })
	tout := b.contenuAttendu() // avant la panne : elle ne vaut que pour UNE requete
	b.faux.RazCompteurs()
	b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleFilm, Latence: 150 * time.Millisecond, Fois: 1})

	premiere := b.getEnFond("iptva:2001", "s1", "bytes=0-999")
	attendre(t, func() bool { return b.faux.Compteurs().Films == 1 }, "premiere plage ouverte chez le fournisseur")
	rep2, corps2, err2 := b.get("iptva:2001", "s1", "bytes=-500") // l'index, en fin de fichier
	r1 := <-premiere

	if r1.err != nil || r1.rep.StatusCode != 206 || !bytes.Equal(r1.corps, tout[:1000]) {
		t.Errorf("premiere plage annulee par la seconde : statut %d, %d octets, %v", r1.rep.StatusCode, len(r1.corps), r1.err)
	}
	if err2 != nil || rep2.StatusCode != 206 || !bytes.Equal(corps2, tout[len(tout)-500:]) {
		t.Errorf("seconde plage : statut %d, %d octets, %v", rep2.StatusCode, len(corps2), err2)
	}
	if n := b.instr.valeur("vod_erreurs_total"); n != 0 {
		t.Errorf("vod_erreurs_total = %v", n)
	}
	// Les deux lectures se sont chevauchees : elles ont partage le bail de la session.
	if n := b.baux.bail("s1").libere.Load(); n != 1 || len(b.baux.acquisitions()) != 2 {
		t.Errorf("bail rendu %d fois pour %d acquisitions, attendu 1 et 2", n, len(b.baux.acquisitions()))
	}
}

// Une AUTRE ressource (zapping film -> film) annule la lecture en cours tout de suite, sans
// attendre la grace : la lecture bloquee par la contre-pression ne finirait jamais seule.
func TestGrace_UneAutreRessourceAnnuleToutDeSuite(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{TailleFilm: grosFilm}, func(r *Reglages) { r.GraceAnnulation = 2 * time.Second })
	ancienne := b.ouvrirBloque("bytes=0-", 1024)
	time.Sleep(50 * time.Millisecond) // la contre-pression s'etablit

	debut := time.Now()
	rep, corps, err := b.get("iptva:501.mp4", "s1", "bytes=0-9")
	if err != nil || rep.StatusCode != 206 || len(corps) != 10 {
		t.Fatalf("zapping : statut %d, %d octets, %v", rep.StatusCode, len(corps), err)
	}
	if d := time.Since(debut); d >= time.Second {
		t.Errorf("le zapping a attendu %v : une autre ressource doit etre annulee sans grace", d)
	}
	if _, err := io.ReadAll(ancienne.Body); err == nil {
		t.Error("l'ancienne lecture a ete servie jusqu'au bout au lieu d'etre coupee")
	}
}

// La MEME ressource bloquee par la contre-pression est bien annulee, mais seulement une
// fois la grace ecoulee : la nouvelle plage est servie exactement.
func TestGrace_LaMemeRessourceBloqueeEstAnnuleeApresLaGrace(t *testing.T) {
	const grace = 300 * time.Millisecond
	b := nouveauBanc(t, fauxiptv.Options{TailleFilm: grosFilm}, func(r *Reglages) { r.GraceAnnulation = grace })
	ancienne := b.ouvrirBloque("bytes=0-", 1024)
	attendu := b.contenuFaux(grosFilm)
	b.faux.UneSeuleConnexion(0)
	time.Sleep(50 * time.Millisecond)

	debut := time.Now()
	rep, err := b.getPlage("iptva:2001", "s1", "bytes=2000000-2000999")
	if err != nil || rep.StatusCode != 206 {
		t.Fatalf("seek : %v, %v", rep, err)
	}
	defer rep.Body.Close()
	corps, err := io.ReadAll(rep.Body)
	if err != nil || !bytes.Equal(corps, attendu[2000000:2001000]) {
		t.Fatalf("plage inexacte (%d octets, %v)", len(corps), err)
	}
	if d := time.Since(debut); d < grace {
		t.Errorf("nouvelle plage servie apres %v : l'ancienne a ete annulee avant la grace (%v)", d, grace)
	}
	if _, err := io.ReadAll(ancienne.Body); err == nil {
		t.Error("l'ancienne lecture bloquee n'a pas ete coupee apres la grace")
	}
}
