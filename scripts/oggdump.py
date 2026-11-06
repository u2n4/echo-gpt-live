#!/usr/bin/env python3
"""Dump the Ogg/Opus structure of a recording: header, page granule positions and packet TOC bytes.

Written because a recording of OpenAI's voice decoded to digital silence while carrying ~103 bytes
per packet, which is far more data than silence needs. That contradiction is a container question,
not an audio question, so it needs the bytes rather than another decode.
"""
import struct
import sys


def pages(data):
    off = 0
    while off < len(data):
        if data[off:off + 4] != b"OggS":
            nxt = data.find(b"OggS", off + 1)
            if nxt < 0:
                return
            off = nxt
            continue
        if len(data) - off < 27:
            raise ValueError("truncated Ogg page header")
        ver, htype, granule, serial, seq, crc, nseg = struct.unpack_from("<BBqIIIB", data, off + 4)
        segtab = data[off + 27:off + 27 + nseg]
        if len(segtab) != nseg:
            raise ValueError("truncated Ogg segment table")
        body = off + 27 + nseg
        size = sum(segtab)
        if body + size > len(data):
            raise ValueError("truncated Ogg page body")
        yield htype, granule, serial, seq, segtab, data[body:body + size]
        off = body + size


# RFC 6716 section 3.1: the TOC config number names the mode, the audio bandwidth and the frame
# size. Getting this exact matters, because "wideband" and "super-wideband" are the difference
# between a telephone and a voice.
BAND_HZ = {"narrowband": "4 kHz", "mediumband": "6 kHz", "wideband": "8 kHz",
           "super-wideband": "12 kHz", "fullband": "20 kHz"}


def decode_config(c):
    if c < 12:
        band = ["narrowband", "mediumband", "wideband"][c // 4]
        frame = ["10 ms", "20 ms", "40 ms", "60 ms"][c % 4]
        return "SILK", band, frame
    if c < 16:
        band = "super-wideband" if c < 14 else "fullband"
        frame = ["10 ms", "20 ms"][c % 2]
        return "hybrid", band, frame
    band = ["narrowband", "wideband", "super-wideband", "fullband"][(c - 16) // 4]
    frame = ["2.5 ms", "5 ms", "10 ms", "20 ms"][(c - 16) % 4]
    return "CELT-only", band, frame


def main(path, limit=12):
    data = open(path, "rb").read()
    print(f"{path}  {len(data)} bytes")
    npages = 0
    packet_sizes = []
    tocs = []
    for htype, granule, serial, seq, segtab, body in pages(data):
        npages += 1
        if body[:8] == b"OpusHead":
            ver = body[8]
            ch = body[9]
            preskip = struct.unpack_from("<H", body, 10)[0]
            rate = struct.unpack_from("<I", body, 12)[0]
            gain = struct.unpack_from("<h", body, 16)[0]
            fam = body[18]
            print(f"  OpusHead: version={ver} channels={ch} pre_skip={preskip} "
                  f"input_rate={rate} gain={gain} mapping_family={fam}")
            continue
        if body[:8] == b"OpusTags":
            print("  OpusTags present")
            continue
        # one packet per segment run; these recordings write one packet per page
        pos = 0
        for n in segtab:
            pkt = body[pos:pos + n]
            pos += n
            if not pkt:
                continue
            packet_sizes.append(len(pkt))
            toc = pkt[0]
            config = toc >> 3
            stereo = (toc >> 2) & 1
            frames = toc & 0x3
            tocs.append((config, stereo, frames))
        if npages <= limit:
            print(f"  page seq={seq} granule={granule} segs={len(segtab)} bytes={len(body)}")

    if not packet_sizes:
        print("  no audio packets")
        return
    uniq = sorted(set(tocs))
    print(f"  pages={npages} packets={len(packet_sizes)}")
    print(f"  payload bytes: min={min(packet_sizes)} max={max(packet_sizes)} "
          f"mean={sum(packet_sizes) / len(packet_sizes):.1f}")
    print(f"  distinct TOC (config, stereo, framecount): {uniq[:8]}")
    for c, st, fc in uniq:
        mode, band, frame = decode_config(c)
        print(f"    config {c:2d}: {mode} {band}  {frame}  "
              f"{'stereo' if st else 'mono'}  -> audio bandwidth {BAND_HZ[band]}")


if __name__ == "__main__":
    main(sys.argv[1], int(sys.argv[2]) if len(sys.argv) > 2 else 12)
