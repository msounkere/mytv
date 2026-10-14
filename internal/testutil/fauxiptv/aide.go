package fauxiptv

import (
	"bytes"
	"io"
)

type lecteurOctets interface {
	io.ReadSeeker
}

func bytesReader(b []byte) lecteurOctets { return bytes.NewReader(b) }
