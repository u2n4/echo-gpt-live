//go:build linux && cgo

package bridge

/*
#cgo LDFLAGS: -ldl
#include <dlfcn.h>
#include <stdint.h>
#include <stdlib.h>

typedef void OpusDecoder;
typedef void OpusEncoder;

typedef OpusDecoder* (*fn_decoder_create)(int32_t Fs, int channels, int *error);
typedef int (*fn_decode)(OpusDecoder *st, const unsigned char *data, int32_t len, int16_t *pcm, int frame_size, int decode_fec);
typedef void (*fn_decoder_destroy)(OpusDecoder *st);

typedef OpusEncoder* (*fn_encoder_create)(int32_t Fs, int channels, int application, int *error);
typedef int32_t (*fn_encode)(OpusEncoder *st, const int16_t *pcm, int frame_size, unsigned char *data, int32_t max_data_bytes);
typedef int (*fn_encoder_ctl_int)(OpusEncoder *st, int request, int32_t val);
typedef void (*fn_encoder_destroy)(OpusEncoder *st);

static void *opus_handle = NULL;
static fn_decoder_create p_decoder_create = NULL;
static fn_decode p_decode = NULL;
static fn_decoder_destroy p_decoder_destroy = NULL;
static fn_encoder_create p_encoder_create = NULL;
static fn_encode p_encode = NULL;
static fn_encoder_ctl_int p_encoder_ctl_int = NULL;
static fn_encoder_destroy p_encoder_destroy = NULL;

static int load_libopus(void) {
	if (opus_handle != NULL) return 0;
	const char *names[] = {"libopus.so.0", "libopus.so", NULL};
	for (int i = 0; names[i] != NULL; i++) {
		opus_handle = dlopen(names[i], RTLD_LAZY);
		if (opus_handle != NULL) break;
	}
	if (opus_handle == NULL) return -1;
	p_decoder_create = (fn_decoder_create)dlsym(opus_handle, "opus_decoder_create");
	p_decode = (fn_decode)dlsym(opus_handle, "opus_decode");
	p_decoder_destroy = (fn_decoder_destroy)dlsym(opus_handle, "opus_decoder_destroy");
	p_encoder_create = (fn_encoder_create)dlsym(opus_handle, "opus_encoder_create");
	p_encode = (fn_encode)dlsym(opus_handle, "opus_encode");
	p_encoder_ctl_int = (fn_encoder_ctl_int)dlsym(opus_handle, "opus_encoder_ctl");
	p_encoder_destroy = (fn_encoder_destroy)dlsym(opus_handle, "opus_encoder_destroy");
	if (!p_decoder_create || !p_decode || !p_decoder_destroy ||
	    !p_encoder_create || !p_encode || !p_encoder_ctl_int || !p_encoder_destroy) {
		dlclose(opus_handle);
		opus_handle = NULL;
		return -2;
	}
	return 0;
}

static OpusDecoder* call_decoder_create(int32_t fs, int ch, int *err) {
	return p_decoder_create(fs, ch, err);
}
static int call_decode(OpusDecoder *st, const unsigned char *data, int32_t len, int16_t *pcm, int frame_size) {
	return p_decode(st, data, len, pcm, frame_size, 0);
}
static void call_decoder_destroy(OpusDecoder *st) {
	p_decoder_destroy(st);
}
static OpusEncoder* call_encoder_create(int32_t fs, int ch, int app, int *err) {
	return p_encoder_create(fs, ch, app, err);
}
static int32_t call_encode(OpusEncoder *st, const int16_t *pcm, int frame_size, unsigned char *data, int32_t max_bytes) {
	return p_encode(st, pcm, frame_size, data, max_bytes);
}
static int call_encoder_ctl_int(OpusEncoder *st, int req, int32_t val) {
	return p_encoder_ctl_int(st, req, val);
}
static void call_encoder_destroy(OpusEncoder *st) {
	p_encoder_destroy(st);
}
*/
import "C"

import (
	"fmt"
	"sync"
	"unsafe"
)

const (
	opusApplicationVOIP  = 2048
	opusSetBitrateReq    = 4002
	opusSetComplexityReq = 4010
	opusSetInbandFECReq  = 4012
	opusSetSignalReq     = 4024
	opusSignalVoice      = 3001
)

var (
	opusLoadOnce sync.Once
	opusLoadErr  error
)

type libopusCodec struct {
	dec unsafe.Pointer
	enc unsafe.Pointer
}

func openOpusCodec(sampleRate, channels int) (opusCodec, error) {
	opusLoadOnce.Do(func() {
		if rc := C.load_libopus(); rc != 0 {
			opusLoadErr = fmt.Errorf("libopus.so.0 not available (rc=%d)", int(rc))
		}
	})
	if opusLoadErr != nil {
		return nil, opusLoadErr
	}
	var derr C.int
	dec := C.call_decoder_create(C.int32_t(sampleRate), C.int(channels), &derr)
	if dec == nil || derr != 0 {
		return nil, fmt.Errorf("opus_decoder_create failed (%d)", int(derr))
	}
	var eerr C.int
	enc := C.call_encoder_create(C.int32_t(sampleRate), C.int(channels), C.int(opusApplicationVOIP), &eerr)
	if enc == nil || eerr != 0 {
		C.call_decoder_destroy(dec)
		return nil, fmt.Errorf("opus_encoder_create failed (%d)", int(eerr))
	}
	// Match Echo/WebRTC speech settings: 32 kbps VBR voice with in-band FEC.
	_ = C.call_encoder_ctl_int(enc, opusSetBitrateReq, 32000)
	_ = C.call_encoder_ctl_int(enc, opusSetInbandFECReq, 1)
	_ = C.call_encoder_ctl_int(enc, opusSetSignalReq, opusSignalVoice)
	_ = C.call_encoder_ctl_int(enc, opusSetComplexityReq, 5)
	return &libopusCodec{dec: dec, enc: enc}, nil
}

func (c *libopusCodec) Decode(payload []byte, pcm []int16) (int, error) {
	if len(payload) == 0 || len(pcm) == 0 {
		return 0, fmt.Errorf("empty buffer")
	}
	n := C.call_decode(
		c.dec,
		(*C.uchar)(unsafe.Pointer(&payload[0])),
		C.int32_t(len(payload)),
		(*C.int16_t)(unsafe.Pointer(&pcm[0])),
		C.int(len(pcm)),
	)
	if n < 0 {
		return 0, fmt.Errorf("opus_decode error %d", int(n))
	}
	return int(n), nil
}

func (c *libopusCodec) Encode(pcm []int16, out []byte) (int, error) {
	if len(pcm) == 0 || len(out) == 0 {
		return 0, fmt.Errorf("empty buffer")
	}
	n := C.call_encode(
		c.enc,
		(*C.int16_t)(unsafe.Pointer(&pcm[0])),
		C.int(len(pcm)),
		(*C.uchar)(unsafe.Pointer(&out[0])),
		C.int32_t(len(out)),
	)
	if n < 0 {
		return 0, fmt.Errorf("opus_encode error %d", int(n))
	}
	return int(n), nil
}

func (c *libopusCodec) Close() {
	if c.dec != nil {
		C.call_decoder_destroy(c.dec)
		c.dec = nil
	}
	if c.enc != nil {
		C.call_encoder_destroy(c.enc)
		c.enc = nil
	}
}
