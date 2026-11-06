#!/usr/bin/env python3
"""Look for the fingerprint of noise suppression and low-rate coding in a decoded recording.

The owner described the voice as sounding "like her microphone is trying to isolate the surrounding
sound, although there is no surrounding sound". That is a description of an artefact, not of a
level or a bandwidth, so it needs a measurement shaped like the artefact.

Three things are measured, per band:

  floor_gap   How far the quiet moments sit below the loud ones. A spectral gate drives the gaps to
              digital silence, so a very large gap in the high bands with a normal gap low down is
              the signature of suppression rather than of a quiet room.

  pumping     How much the band's level jumps between neighbouring 20 ms frames DURING speech. Real
              speech moves smoothly; a gate or an aggressive noise reduction chatters.

  hf_ratio    High band energy against the telephone band, frame by frame, so a high band that
              switches on and off with the voice is visible as variance rather than as an average.

Reads a WAV. Standard library only.
"""
import cmath
import math
import statistics
import struct
import sys
import wave

SIZE = 1024  # ~21 ms at 48 kHz, one Opus frame
BANDS = [(80, 300), (300, 1000), (1000, 3400), (3400, 7000), (7000, 12000)]


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


def db(x):
    return 10 * math.log10(x) if x > 0 else -200.0


def main(path):
    try:
        with wave.open(path) as w:
            rate, n = w.getframerate(), w.getnframes()
            if w.getnchannels() != 1 or w.getsampwidth() != 2 or rate <= 0:
                sys.exit("expected mono 16-bit PCM with a positive sample rate")
            if n > rate * 1800:
                sys.exit("WAV exceeds the 30-minute analysis limit; split it first")
            raw = w.readframes(n)
            if len(raw) != 2 * n:
                sys.exit("truncated WAV data")
            data = struct.unpack(f"<{n}h", raw)
    except (OSError, wave.Error, EOFError) as e:
        sys.exit(f"cannot read WAV: {e}")

    window = [0.5 - 0.5 * math.cos(2 * math.pi * i / (SIZE - 1)) for i in range(SIZE)]
    binhz = rate / SIZE

    frames = []  # (overall_db, [band_db...])
    for start in range(0, n - SIZE, SIZE):
        frame = data[start:start + SIZE]
        spec = fft([complex(frame[i] * window[i]) for i in range(SIZE)])
        power = [abs(spec[k]) ** 2 for k in range(SIZE // 2)]
        total = sum(power)
        bands = []
        for lo, hi in BANDS:
            i0, i1 = int(lo / binhz), min(len(power), int(hi / binhz) + 1)
            bands.append(db(sum(power[i0:i1]) + 1e-12))
        frames.append((db(total + 1e-12), bands))

    if not frames:
        print("file too short")
        return
    overall = sorted(f[0] for f in frames)
    loud_cut = overall[int(len(overall) * 0.80)]
    quiet_cut = overall[int(len(overall) * 0.20)]
    loud = [f for f in frames if f[0] >= loud_cut]
    quiet = [f for f in frames if f[0] <= quiet_cut]

    print(f"{path}   {n / rate:.1f} s, {len(frames)} frames of {SIZE / rate * 1000:.0f} ms")
    print(f"  loud frames {len(loud)}, quiet frames {len(quiet)}")
    print(f"  {'band':>14} {'loud dB':>9} {'quiet dB':>9} {'floor_gap':>10} {'pumping':>9}")
    for bi, (lo, hi) in enumerate(BANDS):
        lmed = statistics.median(f[1][bi] for f in loud)
        qmed = statistics.median(f[1][bi] for f in quiet)
        # frame-to-frame movement inside speech only
        deltas = [abs(loud[i][1][bi] - loud[i - 1][1][bi]) for i in range(1, len(loud))]
        pump = statistics.median(deltas) if deltas else 0.0
        print(f"  {lo:6d}-{hi:5d} {lmed:9.1f} {qmed:9.1f} {lmed - qmed:10.1f} {pump:9.1f}")

    # how steadily the high band tracks the voice
    ratios = [f[1][4] - f[1][2] for f in loud]  # 7-12 kHz minus 1-3.4 kHz, per frame
    if ratios:
        print(f"  high band (7-12k) relative to 1-3.4k during speech: "
              f"median {statistics.median(ratios):.1f} dB, "
              f"spread {statistics.pstdev(ratios):.1f} dB")
    print("  reading: a floor_gap far larger in the high bands than low down, or pumping above ~6 dB,")
    print("  is suppression or gating. Smooth speech sits near 3-5 dB of frame-to-frame movement.")


if __name__ == "__main__":
    if len(sys.argv) != 2:
        sys.exit("usage: ns-signature.py <mono-16bit.wav>")
    main(sys.argv[1])
