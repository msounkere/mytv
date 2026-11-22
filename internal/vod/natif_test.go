package vod

import (
	"bytes"
	"io"
	"net/http"
	"testing"

	"mytv/internal/domain"
	"mytv/internal/testutil/fauxiptv"
)

// Lot 11 : une box IPTV (client « natif ») recoit le fichier TEL QUEL, quel que soit son format :
// ni sonde ni ffmpeg, octets exacts, type d'apres l'extension, Range relaye.
func TestClientNatifRecoitLeFichierTelQuel(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, nil)
	b.sondeur.sonde = domain.Sonde{Conteneur: "avi", CodecVideo: "mpeg2video", CodecAudio: "dts"} // illisible pour un navigateur
	rep, corps := b.getCast("iptva:2002", "s1", ClientNatif)
	if rep.StatusCode != 200 {
		t.Fatalf("statut %d", rep.StatusCode)
	}
	attendu, err := http.Get(b.faux.URL() + "/movie/" + b.faux.Utilisateur() + "/" + b.faux.MotDePasse() + "/2002.mkv")
	if err != nil {
		t.Fatal(err)
	}
	defer attendu.Body.Close()
	tout, _ := io.ReadAll(attendu.Body)
	if !bytes.Equal(corps, tout) {
		t.Error("octets differents du fichier du fournisseur")
	}
	if ct := rep.Header.Get("Content-Type"); ct != "video/x-matroska" {
		t.Errorf("Content-Type = %q", ct)
	}
	if rep.Header.Get("X-Mytv-Mode") != "direct" || len(b.lanceur.liste()) != 0 {
		t.Errorf("mode %q, %d ffmpeg", rep.Header.Get("X-Mytv-Mode"), len(b.lanceur.liste()))
	}
	if b.instr.valeur("vod_relais_natif_total") != 1 {
		t.Error("relais natif non compte")
	}
	if len(b.sondeur.appels) != 0 {
		t.Errorf("sonde appelee pour un client natif : %v", b.sondeur.appels)
	}
	// Deplacement de la box : Range relaye, octets exacts.
	req, _ := http.NewRequest("GET", b.http.URL+"/?id=iptva:2002", nil)
	req.Header.Set("X-Session", "s1")
	req.Header.Set("X-Mytv-Client", ClientNatif)
	req.Header.Set("Range", "bytes=1000-1999")
	r2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	m, _ := io.ReadAll(r2.Body)
	if r2.StatusCode != 206 || !bytes.Equal(m, tout[1000:2000]) {
		t.Errorf("Range : statut %d, %d octets", r2.StatusCode, len(m))
	}
}
