package live

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"mytv/internal/remux"
)

// Mode est la facon dont une chaine du direct est servie. Ses valeurs sont celles de
// l'en-tete X-Mytv-Mode de la playlist.
type Mode string

const (
	ModeDirect Mode = "direct" // le relais d'origine : segments du fournisseur, rien ne change
	ModeAudio  Mode = "audio"  // video copiee, audio reencode en AAC stereo (ffmpeg)
	ModeVideo  Mode = "video"  // video reencodee en H.264, audio en AAC (ffmpeg, ~1 coeur)
	ModeRefus  Mode = "refus"  // illisible et trop cher a convertir : 415
)

// Decision est le choix fait a partir d'une sonde. Raison est un texte pour le journal
// et pour l'utilisateur ; il ne contient que des noms de codecs et une hauteur.
type Decision struct {
	Mode   Mode
	Raison string
	// Code est le code d'erreur d'un refus (format_hevc_non_lisible, format_non_lisible).
	Code string
}

// Capacites : ce que le navigateur de l'appelant sait decoder en plus de h264+aac. Annoncees
// par l'en-tete X-Mytv-Capacites ou ?cap= (hls.js n'ajoute pas toujours un en-tete aux
// requetes de playlist). Absence = incapable de tout : la conversion ne sert que si le
// navigateur ne sait pas lire la source, car elle coute ~1 coeur.
type Capacites struct {
	HEVC bool
	EAC3 bool
	AC3  bool
}

// LireCapacites lit les deux sources et en fait l'union ; une valeur inconnue est ignoree.
func LireCapacites(req *http.Request) Capacites {
	var c Capacites
	valeurs := append([]string{req.URL.Query().Get("cap")}, req.Header.Values("X-Mytv-Capacites")...)
	for _, v := range valeurs {
		for _, m := range strings.Split(v, ",") {
			switch strings.ToLower(strings.TrimSpace(m)) {
			case "hevc":
				c.HEVC = true
			case "eac3":
				c.EAC3 = true
			case "ac3":
				c.AC3 = true
			}
		}
	}
	return c
}

// String donne la forme canonique (journal, en-tete de reponse) ; « aucune » si vide.
func (c Capacites) String() string {
	var l []string
	if c.HEVC {
		l = append(l, "hevc")
	}
	if c.EAC3 {
		l = append(l, "eac3")
	}
	if c.AC3 {
		l = append(l, "ac3")
	}
	if len(l) == 0 {
		return "aucune"
	}
	return strings.Join(l, ",")
}

// audioLisibleAvec : l'audio passe tel quel pour ce client.
func audioLisibleAvec(audio string, c Capacites) bool {
	return audio == "" || audioLisible[audio] || (audio == "eac3" && c.EAC3) || (audio == "ac3" && c.AC3)
}

// ReglagesDecision : les deux reglages qui bornent le reencodage video.
type ReglagesDecision struct {
	ReencodageVideo bool // live.reencodage_video
	HauteurMax      int  // live.hauteur_max_reencodage
}

// Audio et video que hls.js decode dans un navigateur, en MPEG-TS. Tout autre audio
// (ac3, eac3, dts, truehd, mp2...) est reencode : le laisser passer donne un
// fragParsingError en boucle (2946 erreurs mesurees en recette). Meme logique que
// l'ancien service (strategie.decider), moins ses codecs de conteneurs qui n'existent pas en TS.
var (
	audioLisible = map[string]bool{"aac": true, "mp3": true}
	videoLisible = map[string]bool{"h264": true, "avc1": true}
)

// nomCodecSur : le codec vient de la sonde, donc d'un flux du fournisseur ; il n'entre
// dans un message que s'il a la forme d'un nom de codec.
var nomCodecSur = regexp.MustCompile(`^[a-z0-9_]{1,20}$`)

// DeciderLive choisit le mode d'une chaine. Fonction pure.
//
// POURQUOI la hauteur maximale : le reencodage HEVC -> H.264 est logiciel (pas de GPU) et
// tient le temps reel jusqu'au 1080p seulement (mesure de l'ancien service : ~1 coeur).
// Au-dela (4K, 16 Mo par segment), il saturerait la machine sans jamais suivre le direct :
// mieux vaut le dire tout de suite que de bloquer un processeur pour rien.
func DeciderLive(s remux.SondeDirect, c Capacites, r ReglagesDecision) Decision {
	video := strings.ToLower(s.CodecVideo)
	audio := strings.ToLower(s.CodecAudio)

	// Flux sonore seul (radio) : rien a reencoder cote image.
	if video == "" {
		if audioLisibleAvec(audio, c) {
			return Decision{Mode: ModeDirect, Raison: "flux audio seul lisible tel quel"}
		}
		return Decision{Mode: ModeAudio, Raison: fmt.Sprintf("flux audio seul en %s, reencode en aac", nomPourMessage(audio))}
	}

	if (video == "hevc" || video == "h265") && c.HEVC {
		// Navigateur capable : aucune conversion, 4K comprise (le relais d'origine, sans ffmpeg).
		if !audioLisibleAvec(audio, c) {
			return Decision{Mode: ModeAudio, Raison: fmt.Sprintf("audio en %s illisible par ce navigateur, converti en aac (video hevc copiee)", nomPourMessage(audio))}
		}
		return Decision{Mode: ModeDirect, Raison: "hevc decodable par ce navigateur, relaye tel quel"}
	}
	if !videoLisible[video] {
		nom := nomPourMessage(video)
		code := "format_non_lisible"
		if video == "hevc" || video == "h265" {
			code = "format_hevc_non_lisible"
		}
		taille := nom
		if s.Hauteur > 0 {
			taille = fmt.Sprintf("%s %dp", nom, s.Hauteur)
		}
		switch {
		case !r.ReencodageVideo:
			return Decision{Mode: ModeRefus, Code: code,
				Raison: fmt.Sprintf("video en %s non lisible par un navigateur : le reencodage video est desactive sur ce serveur", taille)}
		case s.Hauteur > r.HauteurMax:
			return Decision{Mode: ModeRefus, Code: code,
				Raison: fmt.Sprintf("video en %s non lisible par un navigateur : au-dela de %dp le reencodage logiciel ne tient pas le temps reel", taille, r.HauteurMax)}
		}
		return Decision{Mode: ModeVideo, Raison: fmt.Sprintf("video en %s, reencodee en h264 (cout : environ un coeur)", taille)}
	}

	if !audioLisibleAvec(audio, c) {
		return Decision{Mode: ModeAudio, Raison: fmt.Sprintf("audio en %s illisible par un navigateur, converti en aac", nomPourMessage(audio))}
	}
	return Decision{Mode: ModeDirect, Raison: "h264 et audio lisibles tels quels"}
}

func nomPourMessage(codec string) string {
	if nomCodecSur.MatchString(codec) {
		return codec
	}
	return "inconnu"
}
