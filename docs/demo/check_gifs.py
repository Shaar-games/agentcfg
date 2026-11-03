#!/usr/bin/env python3
"""Validate generated README GIFs and enforce the slower recording contract.

Pure Python standard library: no ffprobe/Pillow dependency on CI runners.
"""

from pathlib import Path
from struct import unpack_from

BASELINE_SECONDS = {
    "agentcfg-config.gif": 7.36,
    "agentcfg-list.gif": 4.12,
}


def skip_subblocks(data: bytes, offset: int) -> int:
    while True:
        if offset >= len(data):
            raise ValueError("truncated GIF subblocks")
        size = data[offset]
        offset += 1
        if size == 0:
            return offset
        offset += size
        if offset > len(data):
            raise ValueError("truncated GIF subblock data")


def gif_details(data: bytes) -> tuple[int, int, int, float]:
    if len(data) < 14 or data[:6] not in (b"GIF89a", b"GIF87a"):
        raise ValueError("invalid GIF header")
    width, height = unpack_from("<HH", data, 6)
    offset = 13
    packed = data[10]
    if packed & 0x80:
        offset += 3 * (1 << ((packed & 7) + 1))

    frames = 0
    duration_cs = 0
    frame_delay_cs = 0
    while offset < len(data):
        introducer = data[offset]
        offset += 1
        if introducer == 0x3B:  # Trailer
            break
        if introducer == 0x21:  # Extension
            if offset >= len(data):
                raise ValueError("truncated GIF extension")
            label = data[offset]
            offset += 1
            if label != 0xF9:
                offset = skip_subblocks(data, offset)
                continue
            if offset + 6 > len(data) or data[offset] != 4 or data[offset + 5] != 0:
                raise ValueError("invalid GIF graphic control extension")
            frame_delay_cs = unpack_from("<H", data, offset + 2)[0]
            offset += 6
            continue
        if introducer != 0x2C or offset + 9 > len(data):
            raise ValueError("invalid GIF image descriptor")
        descriptor = data[offset : offset + 9]
        offset += 9
        if descriptor[8] & 0x80:
            offset += 3 * (1 << ((descriptor[8] & 7) + 1))
        if offset >= len(data):
            raise ValueError("truncated GIF image data")
        offset += 1  # LZW minimum code size
        offset = skip_subblocks(data, offset)
        frames += 1
        duration_cs += frame_delay_cs
        frame_delay_cs = 0
    return width, height, frames, duration_cs / 100.0


def main() -> None:
    for name, baseline in BASELINE_SECONDS.items():
        path = Path("docs/demo") / name
        data = path.read_bytes()
        width, height, frames, duration = gif_details(data)
        if width < 600 or height < 300 or frames < 10:
            raise SystemExit(f"{name}: invalid dimensions or frame count")
        if not 20_000 < len(data) < 15_000_000:
            raise SystemExit(f"{name}: invalid file size: {len(data)}")
        if duration + 0.01 < 2 * baseline:
            raise SystemExit(f"{name}: {duration:.2f}s is not twice the {baseline:.2f}s baseline")
        print(f"{name}: {width}x{height}, {frames} frames, {duration:.2f}s, {len(data):,} bytes")


if __name__ == "__main__":
    main()
