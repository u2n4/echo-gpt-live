//go:build !linux || !cgo

package bridge

import "errors"

func openOpusCodec(sampleRate, channels int) (opusCodec, error) {
	return nil, errors.New("libopus dynamic booster requires linux with cgo")
}
