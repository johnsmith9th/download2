# Renders the app icon into icon.ico and appicon.png, both next to this file.
#
# Same style as PMDir's tools/genicon/gen_icon.py: the rounded square with
# conic-gradient(from 210deg, #2e86c1, #16a085, #8e44ad, #2e86c1) and a white glyph
# in the middle. Only the glyph differs - U+21AA here instead of the letter "P".
#
#   F:\miniconda3\envs\mis\python.exe tools\genicon\gen_icon.py
#
# The .rc next door points at the .ico written here, and scripts\build.ps1 compiles that into
# the .syso `go build` links into the exe. Everything the icon is lives in this directory -
# deliberately not under build\, which is artifacts only and gets deleted by -Clean.
import math
import os
import sys

from PIL import Image, ImageDraw, ImageFont

HERE = os.path.dirname(os.path.abspath(__file__))

MASTER = 1024  # everything else is downscaled from this, which is the antialiasing
RATIO = 0.25  # CSS border-radius 6px / 24px
START_DEG = 210.0
STOPS = [(0x2E, 0x86, 0xC1), (0x16, 0xA0, 0x85), (0x8E, 0x44, 0xAD), (0x2E, 0x86, 0xC1)]

GLYPH = "\u21aa"
# Segoe UI Symbol is the only font here with this codepoint that keeps enough stroke
# to survive 16px (Segoe UI itself has no glyph, MS Gothic and Lucida Sans Unicode are
# too thin). It has no bold face, so CSS font-weight: 700 is paid for with a stroke.
FONT = r"C:\Windows\Fonts\seguisym.ttf"
FONT_EM = 0.62  # glyph size / box size; the wider PMDir ratio 13/24 was for one letter
STROKE = 16 / MASTER  # stroke_width / box size, i.e. 2px on a 128px render
ICO_SIZES = [(16, 16), (20, 20), (24, 24), (32, 32), (40, 40), (48, 48), (64, 64), (96, 96), (128, 128), (256, 256)]


def lerp(a, b, t):
    return (a[0] + (b[0] - a[0]) * t, a[1] + (b[1] - a[1]) * t, a[2] + (b[2] - a[2]) * t)


def conic(angle_deg):
    # CSS conic: 0deg at 12 o'clock, clockwise; stops evenly spaced over 120deg
    t = ((angle_deg - START_DEG) % 360.0) / 120.0
    i = int(t)
    return lerp(STOPS[i], STOPS[i + 1], t - i)


def background(size):
    img = Image.new("RGBA", (size, size))
    px = img.load()
    c = (size - 1) / 2.0
    r = RATIO * size
    inner = size / 2.0 - r
    for y in range(size):
        dy = abs(y - c) - inner
        for x in range(size):
            dx = abs(x - c) - inner
            if dx <= 0 or dy <= 0:
                inside = dx <= r and dy <= r  # straight-edge band, still inside the square
            else:
                inside = dx * dx + dy * dy <= r * r  # corner disc
            if not inside:
                continue
            ang = math.degrees(math.atan2(x - c, c - y)) % 360.0
            px[x, y] = tuple(int(v + 0.5) for v in conic(ang)) + (255,)
    return img


def render(size):
    """The icon at `size`, always composed at MASTER and then downscaled.

    Drawing the glyph at the final size is what breaks 16px: the arrow would be
    rasterised from a 10px font with no antialiasing to recover, while the background
    still comes off the 1024 master. Composing once at MASTER keeps both layers on the
    same supersampled footing.
    """
    img = background(MASTER)
    draw = ImageDraw.Draw(img)
    font = ImageFont.truetype(FONT, round(FONT_EM * MASTER))
    stroke = round(STROKE * MASTER)
    # Centred on the glyph's own ink box, not on the font's ascent/descent: a symbol
    # face's vertical metrics are not a good guess at where its arrows sit.
    box = draw.textbbox((0, 0), GLYPH, font=font, stroke_width=stroke)
    draw.text(((MASTER - (box[2] - box[0])) / 2.0 - box[0], (MASTER - (box[3] - box[1])) / 2.0 - box[1]),
              GLYPH, font=font, fill=(255, 255, 255, 255), stroke_width=stroke, stroke_fill=(255, 255, 255, 255))
    return img if size == MASTER else img.resize((size, size), Image.LANCZOS)


def main():
    master = render(MASTER)
    appicon = os.path.join(HERE, "appicon.png")
    ico = os.path.join(HERE, "icon.ico")
    master.save(appicon)
    # One source, ten sizes: PIL resizes per entry, so every size comes off the 1024 master
    master.save(ico, format="ICO", sizes=ICO_SIZES)
    print("wrote", appicon)
    print("wrote", ico, os.path.getsize(ico), "bytes")


if __name__ == "__main__":
    sys.exit(main())
