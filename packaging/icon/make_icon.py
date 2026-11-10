"""Draws Signal Atlas's app icon and writes it in each platform's format.

    python packaging/icon/make_icon.py

icon.png (1024 px, the source), icon.ico (Windows), icon.icns (macOS) and icon-256.png (Linux).
A location pin under three signal arcs, in the app's colours. Needs Pillow.
"""
import math
import os
from PIL import Image, ImageDraw

HERE = os.path.dirname(os.path.abspath(__file__))
BG, PANEL, ACCENT, FG = (17, 22, 20), (24, 31, 28), (82, 194, 176), (232, 238, 234)
S = 4096  # drawn large, then scaled down for smooth edges


def draw():
    im = Image.new("RGBA", (S, S), (0, 0, 0, 0))
    d = ImageDraw.Draw(im)
    m = int(S * 0.06)
    d.rounded_rectangle([m, m, S - m, S - m], radius=int(S * 0.2), fill=BG, outline=(43, 52, 48), width=int(S * 0.012))
    cx, cy = S / 2, S * 0.60
    # pin: a circle drawn into a point
    r = S * 0.15
    tip = (cx, cy + r * 2.05)
    a = math.asin(r / (tip[1] - cy))
    left = (cx - r * math.cos(a), cy + r * math.sin(a))
    right = (cx + r * math.cos(a), cy + r * math.sin(a))
    d.polygon([left, right, tip], fill=ACCENT)
    d.ellipse([cx - r, cy - r, cx + r, cy + r], fill=ACCENT)
    d.ellipse([cx - r * 0.42, cy - r * 0.42, cx + r * 0.42, cy + r * 0.42], fill=BG)
    # signal arcs above the pin
    w = int(S * 0.045)
    for i, rr in enumerate((0.26, 0.37, 0.48)):
        R = S * rr
        box = [cx - R, cy - R, cx + R, cy + R]
        d.arc(box, start=225, end=315, fill=ACCENT if i < 2 else FG, width=w)
    return im


def main():
    big = draw().resize((1024, 1024), Image.LANCZOS)
    big.save(os.path.join(HERE, "icon.png"))
    big.resize((256, 256), Image.LANCZOS).save(os.path.join(HERE, "icon-256.png"))
    big.save(os.path.join(HERE, "icon.ico"), sizes=[(16, 16), (24, 24), (32, 32), (48, 48), (64, 64), (128, 128), (256, 256)])
    big.save(os.path.join(HERE, "icon.icns"))


if __name__ == "__main__":
    main()
