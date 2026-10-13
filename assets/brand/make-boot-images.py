#!/usr/bin/env python3
"""Make the boot images from the logo masters (needs Python 3 + Pillow).

    python3 assets/brand/make-boot-images.py

Writes (all committed, so servers don't need Pillow):
  assets/brand/grub/background.png   1920x1080, installed server's boot menu
  assets/brand/grub/select_c.png     highlight of the chosen menu line
  assets/brand/plymouth/watermark.png  logo on the boot splash
  assets/brand/iso/splash.png        640x480, installer CD/USB boot menu (BIOS + UEFI)
"""
import os
from PIL import Image, ImageDraw, ImageFont

HERE = os.path.dirname(os.path.abspath(__file__))
STACKED = os.path.join(HERE, "master", "p8s-primary-stacked-white_8K.png")
TOP, BOTTOM = (1, 50, 88), (1, 20, 36)  # brand blue #014578, darkened toward the bottom
FONT = "/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf"


def gradient(w, h):
    im = Image.new("RGB", (w, h))
    d = ImageDraw.Draw(im)
    for y in range(h):
        t = y / max(h - 1, 1)
        d.line([(0, y), (w, y)], fill=tuple(round(a + (b - a) * t) for a, b in zip(TOP, BOTTOM)))
    return im


def logo(width):
    lg = Image.open(STACKED).convert("RGBA")
    return lg.resize((width, round(lg.height * width / lg.width)), Image.LANCZOS)


def put(bg, lg, cx, top):
    bg.paste(lg, (round(cx - lg.width / 2), round(top)), lg)


def text(img, s, size, cy, fill):
    try:
        f = ImageFont.truetype(FONT, size)
    except OSError:
        f = ImageFont.load_default()
    d = ImageDraw.Draw(img)
    w = d.textlength(s, font=f)
    d.text(((img.width - w) / 2, cy), s, font=f, fill=fill)


def out(*p):
    path = os.path.join(HERE, *p)
    os.makedirs(os.path.dirname(path), exist_ok=True)
    return path


# GRUB (installed server): logo in the top third, the menu below it (theme.txt)
bg = gradient(1920, 1080)
put(bg, logo(560), 960, 150)
text(bg, "OS", 34, 445, (200, 215, 230))
bg.save(out("grub", "background.png"), optimize=True)
sel = Image.new("RGBA", (8, 8), (90, 167, 224, 90))
sel.save(out("grub", "select_c.png"))

# Plymouth splash: the logo alone, on the same blue (BackgroundStartColor)
logo(520).save(out("plymouth", "watermark.png"), optimize=True)

# Installer boot menu (ISOLINUX vesamenu / GRUB on the CD): 640x480
sp = gradient(640, 480)
put(sp, logo(250), 320, 34)
text(sp, "OS installer", 15, 166, (200, 215, 230))
sp.convert("RGB").save(out("iso", "splash.png"), optimize=True)
print("boot images written")
