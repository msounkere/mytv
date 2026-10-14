package remux

import (
	"fmt"
	"strings"

	"mytv/internal/domain"
)

// ModeRefus : valeur de Decision.Mode quand le navigateur ne pourra pas lire le
// film et que le serveur ne transcode pas la video (cout CPU, aucun mode
// correspondant dans le contrat). L'appelant repond « 415 ».
const ModeRefus domain.Mode = ""

// Codecs que les navigateurs decodent et que le mp4 accepte tels quels. Les
// autres codecs audio sont re-encodes en AAC. La liste est celle du service Python
// actuel, sans vorbis/flac : le muxer mp4 de ffmpeg les refuse sans option
// experimentale, un « -c copy » echouerait donc au lieu de jouer.
var (
	audioCopiable = map[string]bool{"aac": true, "mp3": true, "opus": true}
	videoLisible  = map[string]bool{"h264": true, "avc1": true}
)

// Decider choisit le mode de lecture pour un navigateur de bureau. Fonction pure.
func Decider(s *domain.Sonde) domain.Decision {
	if s == nil {
		return domain.Decision{Mode: ModeRefus, Raison: "analyse absente"}
	}
	video := strings.ToLower(s.CodecVideo)
	audio := strings.ToLower(s.CodecAudio)
	if video == "" && audio == "" {
		return domain.Decision{Mode: ModeRefus, Raison: "aucun flux audio ni video reconnu"}
	}
	if video != "" && !videoLisible[video] {
		return domain.Decision{Mode: ModeRefus,
			Raison: fmt.Sprintf("video en %s non lisible par un navigateur (pas de transcodage video)", video)}
	}
	if audio != "" && !audioCopiable[audio] {
		return domain.Decision{Mode: domain.ModeAudioConverti,
			Raison: fmt.Sprintf("audio en %s illisible par un navigateur, converti en aac", audio)}
	}
	if conteneurNavigateur(s.Conteneur) {
		return domain.Decision{Mode: domain.ModeDirect, Raison: "mp4 lisible tel quel"}
	}
	return domain.Decision{Mode: domain.ModeRemux,
		Raison: fmt.Sprintf("conteneur %s remuxe en mp4 fragmente", nomConteneur(s.Conteneur))}
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
