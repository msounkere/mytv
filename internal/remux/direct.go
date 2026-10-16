package remux

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"mytv/internal/domain"
)

// SondeDirect est ce que la sonde d'un segment du direct a vu. La hauteur manque a
// domain.Sonde (les films n'en ont pas besoin) : elle decide, ici, entre reencoder et
// refuser une video HEVC.
type SondeDirect struct {
	CodecVideo string // vide : flux audio seul
	Hauteur    int    // 0 : inconnue
	CodecAudio string // vide : aucun son
}

// SonderDirect analyse les octets d'UN segment avec ffprobe. Comme pour les films,
// ffprobe ne lit jamais l'adresse du fournisseur : il lit une adresse locale a usage
// unique alimentee par `src` (voir fluxSonde), donc ni identifiant dans argv, ni
// redirection suivie sans le filtre d'hotes.
//
// Pas de cache ici : le verdict d'une chaine se garde cote direct (duree propre, echec
// compris), et ce Sondeur n'a pas de notion de chaine.
func (s *Sondeur) SonderDirect(ctx context.Context, src domain.SourceOctets) (SondeDirect, error) {
	if src == nil {
		return SondeDirect{}, fmt.Errorf("%w : source d'octets absente", ErrArgumentRefuse)
	}
	sortie, err := s.executerSonde(ctx, src)
	if err != nil {
		return SondeDirect{}, err
	}
	return AnalyserSondeDirect(sortie)
}

// AnalyserSondeDirect lit la sortie JSON de ffprobe (-show_streams). Le premier flux
// video et le premier flux audio comptent ; les pochettes (mjpeg, png, bmp) ne sont
// pas une video.
func AnalyserSondeDirect(sortie []byte) (SondeDirect, error) {
	var d struct {
		Streams []struct {
			Type    string `json:"codec_type"`
			Codec   string `json:"codec_name"`
			Hauteur int    `json:"height"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(sortie, &d); err != nil {
		return SondeDirect{}, errors.New("remux : reponse de ffprobe illisible")
	}
	var r SondeDirect
	for _, st := range d.Streams {
		codec := strings.ToLower(st.Codec)
		switch st.Type {
		case "video":
			switch codec {
			case "mjpeg", "png", "bmp":
				continue
			}
			if r.CodecVideo == "" {
				r.CodecVideo, r.Hauteur = codec, st.Hauteur
			}
		case "audio":
			if r.CodecAudio == "" {
				r.CodecAudio = codec
			}
		}
	}
	if r.CodecVideo == "" && r.CodecAudio == "" {
		return SondeDirect{}, errors.New("remux : aucun flux reconnu")
	}
	return r, nil
}

// TacheDirect decrit CE QUE ffmpeg doit produire pour une chaine du direct : jamais
// COMMENT (les arguments sont fabriques ici, a partir de champs valides).
type TacheDirect struct {
	// Source donne le flux MPEG-TS continu de la chaine (segments mis bout a bout). Elle
	// n'est ouverte qu'a l'octet 0 : un direct ne se rejoue pas.
	Source domain.SourceOctets
	// Video : reencoder en H.264 ; sinon la video est copiee. L'audio est toujours reencode
	// en AAC (la copie ne change rien d'utile : un AC3/EAC3 copie est illisible).
	Video bool
	// Dossier est un dossier DEJA cree ; Prefixe distingue les segments de cette execution
	// de ceux d'une execution precedente (des URL de segments jamais reutilisees).
	Dossier string
	Prefixe string
	// SegmentSecondes et ListeSegments : duree d'un segment HLS et taille de la fenetre.
	SegmentSecondes int
	ListeSegments   int
}

// NomPlaylistDirect et le modele de nom des segments produits.
const NomPlaylistDirect = "index.m3u8"

// Arguments de ffmpeg pour un direct. Comme pour les films, l'entree est `pipe:0` : une
// URL du fournisseur porte les identifiants, lisibles de tout utilisateur dans
// /proc/<pid>/cmdline et recopiables dans les erreurs de ffmpeg. Les parametres sont ceux
// de l'ancien service, qu'une recette reelle avait valides : x264 veryfast crf 23, GOP de
// 100 images a intervalle regulier (-sc_threshold 0) pour des segments de duree stable.
func ArgumentsDirect(t TacheDirect) ([]string, error) {
	if t.Source == nil {
		return nil, fmt.Errorf("%w : source d'octets absente", ErrArgumentRefuse)
	}
	if t.SegmentSecondes < 1 || t.SegmentSecondes > 30 || t.ListeSegments < 3 || t.ListeSegments > 30 {
		return nil, fmt.Errorf("%w : duree ou liste de segments hors limites", ErrArgumentRefuse)
	}
	if err := validerCheminSortie(t.Dossier); err != nil {
		return nil, err
	}
	if !prefixeSegmentSur(t.Prefixe) {
		return nil, fmt.Errorf("%w : prefixe de segment invalide", ErrArgumentRefuse)
	}
	// -f mpegts : le format est connu, ffmpeg n'a pas a le deviner sur un tube.
	// -protocol_whitelist pipe : meme un flux piege ne peut pas lui faire ouvrir autre chose.
	a := []string{"-hide_banner", "-loglevel", "warning", "-nostdin", "-protocol_whitelist", protocolesEntree,
		"-fflags", "+genpts", "-f", "mpegts", "-i", "pipe:0", "-map", "0:v:0?", "-map", "0:a:0?", "-sn", "-dn"}
	if t.Video {
		a = append(a, "-c:v", "libx264", "-preset", "veryfast", "-crf", "23", "-g", "100", "-sc_threshold", "0")
	} else {
		a = append(a, "-c:v", "copy")
	}
	a = append(a, "-c:a", "aac", "-b:a", "192k", "-ac", "2",
		"-f", "hls", "-hls_time", strconv.Itoa(t.SegmentSecondes), "-hls_list_size", strconv.Itoa(t.ListeSegments),
		"-hls_flags", "delete_segments+independent_segments",
		"-hls_segment_filename", t.Dossier+"/"+t.Prefixe+"_%05d.ts", t.Dossier+"/"+NomPlaylistDirect)
	return a, nil
}

// prefixeSegmentSur : lettres minuscules et chiffres seulement, de 4 a 16 caracteres.
func prefixeSegmentSur(p string) bool {
	if len(p) < 4 || len(p) > 16 {
		return false
	}
	for _, c := range p {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// validerCheminSortie refuse ce qui changerait le sens d'un argument de ffmpeg : un
// chemin relatif ou commencant par « - », un « % » (modele de nom), un caractere de controle.
func validerCheminSortie(d string) error {
	if !strings.HasPrefix(d, "/") || len(d) > urlMax {
		return fmt.Errorf("%w : dossier de sortie absolu attendu", ErrArgumentRefuse)
	}
	for _, r := range d {
		if r < ' ' || r == 0x7f || r == '%' {
			return fmt.Errorf("%w : dossier de sortie avec caractere interdit", ErrArgumentRefuse)
		}
	}
	return nil
}

// LancerDirect demarre le transcodage d'une chaine, apres avoir obtenu un des
// processus_max emplacements. Il ne rend pas la sortie standard (ffmpeg ecrit des
// fichiers) : l'appelant surveille le dossier et arrete le processus.
func (l *Lanceur) LancerDirect(ctx context.Context, t TacheDirect) (domain.Processus, error) {
	args, err := ArgumentsDirect(t)
	if err != nil {
		return nil, err
	}
	// Reprises a zero : un direct n'a pas d'octet N a rouvrir, une coupure de l'alimenteur
	// est une fin de flux (ffmpeg s'arrete, l'appelant relance proprement).
	return l.lancer(ctx, args, t.Source, 0)
}
