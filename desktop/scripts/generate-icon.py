#!/usr/bin/env python3
"""Generate the Pie Relay app icon as a 1024px PNG (source for `npx tauri icon`).

Deliberately minimal: a dark rounded-square field with two nodes joined by a
relay line — a plain "connection" motif, not an illustration. Rendered at 4x and
downsampled for clean anti-aliasing. No external assets, no network.
"""
from PIL import Image, ImageDraw

S = 1024          # final icon edge
SS = 4            # supersample factor
E = S * SS        # working edge

BG_TOP = (14, 23, 32)      # #0e1720 dark slate
BG_BOT = (23, 37, 54)      # #172536 slightly lifted
ACCENT = (56, 189, 248)    # #38bdf8 relay/connection cyan
NODE_HUB = (226, 240, 252) # near-white central hub
LINE = (86, 160, 205)      # muted cyan link


def lerp(a, b, t):
    return tuple(round(a[i] + (b[i] - a[i]) * t) for i in range(3))


def main(out_path: str) -> None:
    img = Image.new("RGB", (E, E), BG_TOP)
    d = ImageDraw.Draw(img)

    # Vertical gradient background.
    for y in range(E):
        d.line([(0, y), (E, y)], fill=lerp(BG_TOP, BG_BOT, y / E))

    # Three points: two peer nodes + a central relay hub (the "relay" idea).
    cx, cy = E // 2, E // 2
    span = int(E * 0.30)
    left = (cx - span, cy + int(E * 0.14))
    right = (cx + span, cy + int(E * 0.14))
    hub = (cx, cy - int(E * 0.16))

    link_w = int(E * 0.028)
    for pt in (left, right):
        d.line([hub, pt], fill=LINE, width=link_w)

    def disc(center, radius, fill, ring=None):
        x, y = center
        d.ellipse([x - radius, y - radius, x + radius, y + radius], fill=fill)
        if ring:
            rw = int(E * 0.012)
            d.ellipse(
                [x - radius, y - radius, x + radius, y + radius],
                outline=ring,
                width=rw,
            )

    node_r = int(E * 0.075)
    hub_r = int(E * 0.105)
    disc(left, node_r, ACCENT)
    disc(right, node_r, ACCENT)
    disc(hub, hub_r, NODE_HUB, ring=ACCENT)

    img = img.resize((S, S), Image.LANCZOS)
    img.save(out_path, "PNG")
    print(f"wrote {out_path} ({S}x{S})")


if __name__ == "__main__":
    import os
    here = os.path.dirname(os.path.abspath(__file__))
    main(os.path.join(here, "..", "app-icon.png"))
