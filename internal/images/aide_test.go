package images

import (
	"os"

	"mytv/internal/hls"
)

func newSceauAutre() (*hls.Sceau, error) {
	return hls.NouveauSceau([]byte("un-tout-autre-secret-de-32-octets-minimum!"), libelleChiffrement, libelleNonce)
}

// newSceauSegment : le sceau des segments HLS, avec LE MEME secret que le service.
func newSceauSegment() (*hls.Sceau, error) {
	return hls.NouveauSceau(secretTest, "mytv/hls/jeton/chiffrement/v1", "mytv/hls/jeton/nonce/v1")
}

func osStat(p string) (os.FileInfo, error) { return os.Stat(p) }
func osEcrire(p, c string) error           { return os.WriteFile(p, []byte(c), 0o600) }
