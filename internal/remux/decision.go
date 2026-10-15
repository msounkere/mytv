package remux

import (
	"fmt"
	"regexp"
	"strings"

	"mytv/internal/domain"
)

// Codecs que les navigateurs decodent et que le mp4 accepte tels quels. Les
// autres codecs audio sont re-encodes en AAC. La liste est celle du service Python
// actuel, sans vorbis/flac : le muxer mp4 de ffmpeg les refuse sans option
// experimentale, un « -c copy » echouerait donc au lieu de jouer.
var (
	audioCopiable = map[string]bool{"aac": true, "mp3": true, "opus": true}
	videoLisible  = map[string]bool{"h264": true, "avc1": true}
)

// ErrFormat : le refus de Decider, avec de quoi le rendre exploitable par le client.
// Il vaut domain.ErrNonLisible (errors.Is) ; le message d'Error() reste celui de
// toujours. Il ne porte que des noms de codecs (jamais d'URL ni d'identifiant).
type ErrFormat struct {
	Video  string // codec video refuse, vide si la raison est autre (analyse absente...)
	Raison string
}

func (e *ErrFormat) Error() string       { return domain.ErrNonLisible.Error() + " : " + e.Raison }
func (e *ErrFormat) Is(cible error) bool { return cible == domain.ErrNonLisible }

// HEVC : la video est du H.265, que certains televiseurs lisent mais pas les navigateurs.
func (e *ErrFormat) HEVC() bool { return e.Video == "hevc" || e.Video == "h265" }

// CodeAPI et MessageAPI : le code et le texte de la reponse 415 (l'API les lit par
// interface, sans connaitre ce paquet). Le codec n'entre dans le texte que s'il a la
// forme d'un nom de codec : il vient de la sonde, donc d'un fichier du fournisseur.
func (e *ErrFormat) CodeAPI() string {
	if e.HEVC() {
		return "format_hevc_non_lisible"
	}
	return "format_non_lisible"
}

func (e *ErrFormat) MessageAPI() string {
	switch {
	case e.HEVC():
		return "video HEVC (H.265) non lisible dans un navigateur ; un televiseur ou un Chromecast peut la lire"
	case nomCodecSur.MatchString(e.Video):
		return "video " + e.Video + " non lisible dans un navigateur"
	}
	return "format non lisible dans un navigateur"
}

var nomCodecSur = regexp.MustCompile(`^[a-z0-9_]{1,20}$`)

// Decider choisit le mode de lecture pour un navigateur de bureau. Fonction pure.
// Un format que le serveur ne sait pas rendre lisible (video HEVC : pas de
// transcodage video) donne domain.ErrNonLisible, accompagne de sa raison.
func Decider(s *domain.Sonde) (domain.Decision, error) {
	refus := func(raison string) (domain.Decision, error) {
		e := &ErrFormat{Raison: raison}
		if s != nil {
			e.Video = strings.ToLower(s.CodecVideo)
		}
		return domain.Decision{Raison: raison}, e
	}
	if s == nil {
		return refus("analyse absente")
	}
	video := strings.ToLower(s.CodecVideo)
	audio := strings.ToLower(s.CodecAudio)
	if video == "" && audio == "" {
		return refus("aucun flux audio ni video reconnu")
	}
	if video != "" && !videoLisible[video] {
		return refus(fmt.Sprintf("video en %s non lisible par un navigateur (pas de transcodage video)", video))
	}
	if audio != "" && !audioCopiable[audio] {
		return domain.Decision{Mode: domain.ModeAudioConverti,
			Raison: fmt.Sprintf("audio en %s illisible par un navigateur, converti en aac", audio)}, nil
	}
	if conteneurNavigateur(s.Conteneur) {
		return domain.Decision{Mode: domain.ModeDirect, Raison: "mp4 lisible tel quel"}, nil
	}
	return domain.Decision{Mode: domain.ModeRemux,
		Raison: fmt.Sprintf("conteneur %s remuxe en mp4 fragmente", nomConteneur(s.Conteneur))}, nil
}

// conteneurNavigateur : ffprobe annonce « mov,mp4,m4a,3gp,3g2,mj2 » pour la famille
// QuickTime/MP4 ; « matroska,webm » est autre chose.
func conteneurNavigateur(format string) bool {
	for _, f := range strings.Split(strings.ToLower(format), ",") {
		if f == "mp4" || f == "mov" || f == "m4v" {
			return true
		}
	}
	return false
}

func nomConteneur(format string) string {
	if format == "" {
		return "inconnu"
	}
	return strings.Split(strings.ToLower(format), ",")[0]
}

var _ domain.Decideur = Decider
