#!/usr/bin/env python3
"""Measure a recorded direction: level, and above all the audio BANDWIDTH.

"It sounds like a phone call" is a bandwidth statement, not a loudness one. A telephone stops near
3.4 kHz; Opus at 48 kHz can carry 20 kHz. So this builds a long-term average spectrum over every
frame loud enough to be speech, which is stable, instead of sampling one 43 ms frame per second,
which lands on a random vowel and reports nonsense.

Reads a WAV (decode the .ogg with ffmpeg first). Standard library only plus a hand-rolled FFT, so it
runs on a box with no numpy.
"""
import cmath
import math
import struct
import sys
import wave

SIZE = 2048           # ~43 ms at 48 kHz, a power of two
SPEECH_DBFS = -45.0   # below this a frame is silence or room tone, not the voice under test


def fft(a):
    n = len(a)
    if n == 1:
        return a
    even = fft(a[0::2])
    odd = fft(a[1::2])
    out = [0j] * n
    for k in range(n // 2):
        t = cmath.exp(-2j * math.pi * k / n) * odd[k]
        out[k] = even[k] + t
        out[k + n // 2] = even[k] - t
    return out


def dbfs(frame):
    if not frame:
        return -120.0
    rms = math.sqrt(sum(x * x for x in frame) / len(frame))
    return 20 * math.log10(rms / 32768.0) if rms > 0 else -120.0


def band_energy(power, binhz, lo, hi):
    i0 = max(0, int(lo / binhz))
    i1 = min(len(power), int(hi / binhz) + 1)
    return sum(power[i0:i1])


def main(path):
    w = wave.open(path)
    rate = w.getframerate()
    n = w.getnframes()
    data = struct.unpack("<%dh" % n, w.readframes(n))
    w.close()

    window = [0.5 - 0.5 * math.cos(2 * math.pi * i / (SIZE - 1)) for i in range(SIZE)]
    binhz = rate / SIZE
    acc = [0.0] * (SIZE // 2)
    frames = 0
    speech_s = 0.0
    peak = 0.0
    lvls = []

    for start in range(0, n - SIZE, SIZE // 2):
        frame = data[start:start + SIZE]
        lvl = dbfs(frame)
        peak = max(peak, max(abs(x) for x in frame) / 32768.0)
        if lvl < SPEECH_DBFS:
            continue
        lvls.append(lvl)
        speech_s += (SIZE / 2) / rate
        spec = fft([complex(frame[i] * window[i]) for i in range(SIZE)])
        for k in range(SIZE // 2):
            m = abs(spec[k])
            acc[k] += m * m
        frames += 1

    print(f"{path}   {n / rate:.1f} s @ {rate} Hz")
    if frames == 0:
        print("  no frame reached the speech threshold; this direction is silent")
        return
    lvls.sort()
    print(f"  speech frames: {frames}  (~{speech_s:.1f} s of the file)")
    print(f"  level over speech: median {lvls[len(lvls) // 2]:.1f} dBFS, "
          f"loudest {lvls[-1]:.1f} dBFS, sample peak {20 * math.log10(peak):.1f} dBFS")

    total = sum(acc)
    run = 0.0
    marks = {}
    for frac in (0.90, 0.95, 0.99):
        marks[frac] = None
    run = 0.0
    for i, p in enumerate(acc):
        run += p
        for frac in (0.90, 0.95, 0.99):
            if marks[frac] is None and run >= total * frac:
                marks[frac] = i * binhz
    print(f"  long-term rolloff: 90% below {marks[0.90]:.0f} Hz, "
          f"95% below {marks[0.95]:.0f} Hz, 99% below {marks[0.99]:.0f} Hz")

    ref = band_energy(acc, binhz, 300, 3400)  # the telephone band
    print("  energy per band, relative to the 300-3400 Hz telephone band:")
    for lo, hi in ((0, 300), (300, 3400), (3400, 7000), (7000, 12000), (12000, 20000)):
        e = band_energy(acc, binhz, lo, hi)
        rel = 10 * math.log10(e / ref) if e > 0 and ref > 0 else float("-inf")
        bar = "#" * max(0, int(40 + rel))
        print(f"    {lo:6d}-{hi:6d} Hz  {rel:7.1f} dB  {bar}")

    # Deliberately NOT a verdict on codec bandwidth. Where 99% of speech ENERGY sits is a
    # property of the talker and the room as much as of the codec, and a coder can be given a
    # 12 kHz ceiling and still put almost nothing above 4 kHz. The codec's real bandwidth is in
    # the Opus TOC byte, which scripts/oggdump.py reads directly; this line only says where the
    # energy ended up.
    r99 = marks[0.99] or 0
    print(f"  99% of speech energy sits below {r99:.0f} Hz "
          f"(this is NOT the codec bandwidth; read that from the TOC byte with oggdump.py)")


if __name__ == "__main__":
    main(sys.argv[1])
