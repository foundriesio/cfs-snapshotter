#!/usr/bin/env python3
#
# Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
# SPDX-License-Identifier: Apache-2.0

"""Generate the two cfs-snapshotter architecture diagrams as SVG."""
import sys

W, H = 1600, 900
FONT = "Noto Sans, Liberation Sans, DejaVu Sans, Arial, sans-serif"

# palette
NAVY = "#1e3a5f"
INK = "#1f2937"
MUTED = "#4b5563"
BLUE = "#2563eb"
GREEN = "#15803d"
PURPLE = "#7c3aed"
NOTE_BG, NOTE_BD = "#fffbeb", "#f59e0b"
COL = {
    "client": ("#eef4fb", "#9db9dc"),
    "docker": ("#fff4e6", "#f0b36a"),
    "ctrd": ("#e8f0fb", "#7fa6d9"),
    "cfs": ("#f3ecfb", "#b494e0"),
    "ocicfs": ("#eaf7ec", "#8ac79a"),
    "fs": ("#f4f6f8", "#9aa5b1"),
}


class SVG:
    def __init__(self, title, subtitle):
        self.parts = [
            f'<svg xmlns="http://www.w3.org/2000/svg" width="{W}" height="{H}" viewBox="0 0 {W} {H}" font-family="{FONT}">',
            "<defs>",
            f'<marker id="ab" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="8" markerHeight="8" orient="auto-start-reverse"><path d="M0 0L10 5L0 10z" fill="{BLUE}"/></marker>',
            f'<marker id="ag" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="8" markerHeight="8" orient="auto-start-reverse"><path d="M0 0L10 5L0 10z" fill="{GREEN}"/></marker>',
            f'<marker id="ap" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="8" markerHeight="8" orient="auto-start-reverse"><path d="M0 0L10 5L0 10z" fill="{PURPLE}"/></marker>',
            "</defs>",
            f'<rect width="{W}" height="{H}" fill="#ffffff"/>',
            f'<rect width="{W}" height="72" fill="{NAVY}"/>',
            f'<text x="32" y="47" font-size="30" font-weight="700" fill="#fff">{title}</text>',
            f'<text x="{W-32}" y="46" font-size="18" fill="#dbe4f0" text-anchor="end">{subtitle}</text>',
        ]

    def box(self, x, y, w, h, kind, title=None, sub=None, r=10):
        bg, bd = COL[kind]
        self.parts.append(f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="{r}" fill="{bg}" stroke="{bd}" stroke-width="2"/>')
        if title:
            self.parts.append(f'<text x="{x+16}" y="{y+30}" font-size="21" font-weight="700" fill="{INK}">{title}</text>')
        if sub:
            self.text(x + 16, y + 52, sub, 14, MUTED)

    def sbox(self, x, y, w, h, kind, title, lines=(), mono=None):
        bg, bd = COL[kind]
        self.parts.append(f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="8" fill="#ffffff" stroke="{bd}" stroke-width="1.5"/>')
        self.parts.append(f'<text x="{x+14}" y="{y+26}" font-size="17" font-weight="700" fill="{INK}">{title}</text>')
        yy = y + 46
        for ln in lines:
            self.parts.append(f'<text x="{x+14}" y="{yy}" font-size="14" fill="{MUTED}">{ln}</text>')
            yy += 19
        if mono:
            self.parts.append(f'<text x="{x+14}" y="{yy}" font-size="13.5" font-family="DejaVu Sans Mono, Liberation Mono, monospace" fill="{INK}">{mono}</text>')

    def text(self, x, y, s, size=14, color=INK, anchor="start", weight=400, italic=False):
        st = ' font-style="italic"' if italic else ""
        self.parts.append(f'<text x="{x}" y="{y}" font-size="{size}" fill="{color}" text-anchor="{anchor}" font-weight="{weight}"{st}>{s}</text>')

    def arrow(self, pts, color, marker, dashed=False, both=False, width=2.5):
        d = " ".join(f"{'M' if i == 0 else 'L'}{x} {y}" for i, (x, y) in enumerate(pts))
        dash = ' stroke-dasharray="9 6"' if dashed else ""
        start = f' marker-start="url(#{marker})"' if both else ""
        self.parts.append(f'<path d="{d}" fill="none" stroke="{color}" stroke-width="{width}"{dash} marker-end="url(#{marker})"{start}/>')

    def label(self, x, y, s, color, size=14, anchor="middle", weight=600):
        # white halo for readability over lines
        self.parts.append(f'<text x="{x}" y="{y}" font-size="{size}" text-anchor="{anchor}" font-weight="{weight}" stroke="#fff" stroke-width="5" paint-order="stroke" fill="{color}">{s}</text>')

    def badge(self, x, y, s, color):
        self.parts.append(f'<circle cx="{x}" cy="{y}" r="12" fill="{color}"/>')
        self.parts.append(f'<text x="{x}" y="{y+5}" font-size="14" font-weight="700" fill="#fff" text-anchor="middle">{s}</text>')

    def note(self, x, y, w, h, lines):
        self.parts.append(f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="8" fill="{NOTE_BG}" stroke="{NOTE_BD}" stroke-width="1.5"/>')
        yy = y + 24
        for ln in lines:
            self.parts.append(f'<text x="{x+14}" y="{yy}" font-size="14" fill="{INK}">{ln}</text>')
            yy += 19

    def legend(self, y, items):
        self.parts.append(f'<rect x="32" y="{y}" width="{W-64}" height="56" rx="8" fill="#f8fafc" stroke="#cbd5e1" stroke-width="1.5"/>')
        self.text(52, y + 35, "Legend", 18, INK, weight=700)
        x = 160
        for color, marker, dashed, label in items:
            dash = ' stroke-dasharray="9 6"' if dashed else ""
            self.parts.append(f'<path d="M{x} {y+28} L{x+60} {y+28}" stroke="{color}" stroke-width="2.5"{dash} marker-end="url(#{marker})"/>')
            self.text(x + 72, y + 33, label, 15, INK)
            x += 72 + len(label) * 8.2 + 48

    def write(self, path):
        self.parts.append("</svg>")
        with open(path, "w") as f:
            f.write("\n".join(self.parts))


def pull_diagram(path):
    s = SVG("Image Pull and Ingestion", "cfs-snapshotter + ocicfs · how image content reaches the device")

    # Clients
    s.box(40, 120, 270, 470, "client", "Clients")
    s.sbox(56, 166, 238, 70, "client", "Docker client", ["docker pull, docker build"])
    s.sbox(56, 256, 238, 70, "client", "containerd client", ["ctr pull, nerdctl pull"])
    s.sbox(56, 346, 238, 96, "client", "composectl", ["pulls into its own store, then", "installs into containerd", "(Docker fallback)"])
    s.sbox(56, 462, 238, 90, "client", "Update agent", ["drives preload out of band:", "ocicfs pull, hold, release"])

    # dockerd
    s.box(370, 166, 180, 70, "docker", "dockerd", "Docker Engine")

    # containerd
    s.box(610, 120, 300, 330, "ctrd", "containerd")
    s.sbox(626, 166, 268, 70, "ctrd", "Image service", ["image records, unpack"])
    s.sbox(626, 256, 268, 70, "ctrd", "Content store", ["manifest, config, layer blobs"])
    s.sbox(626, 346, 268, 90, "ctrd", "Unpack", ["Prepare each layer, then Apply;", "remote-snapshotter handshake"])

    # cfs-snapshotter
    s.box(970, 120, 290, 330, "cfs", "cfs-snapshotter")
    s.sbox(986, 166, 258, 70, "cfs", "Proxy snapshotter", ["snapshot chain, admission"])
    s.sbox(986, 256, 258, 90, "cfs", "Proxy differ", ["reads layer blobs from the", "content store, writes into ocicfs"])
    s.sbox(986, 366, 258, 70, "cfs", "Never pulls", ["Prepare and Apply never fetch"])

    # ocicfs
    s.box(1320, 120, 240, 420, "ocicfs", "ocicfs")
    s.sbox(1336, 166, 208, 90, "ocicfs", "Composefs image", ["one per image:", "EROFS metadata"])
    s.sbox(1336, 276, 208, 90, "ocicfs", "Shared file CAS", ["sha256-keyed file contents,", "deduplicated across images"])
    s.sbox(1336, 386, 208, 70, "ocicfs", "ocicfs CLI", ["pull, verify, hold, gc"])

    # registry
    s.box(610, 660, 300, 90, "fs", "OCI registry", "manifests, configs, layer blobs")

    # control arrows (blue)
    s.arrow([(294, 201), (370, 201)], BLUE, "ab")
    s.arrow([(550, 201), (626, 201)], BLUE, "ab")
    s.arrow([(294, 291), (626, 291)], BLUE, "ab")
    s.arrow([(294, 391), (610, 391)], BLUE, "ab")
    s.label(440, 384, "install", BLUE, 13)
    s.arrow([(294, 507), (1300, 507), (1300, 421), (1336, 421)], BLUE, "ab")
    s.label(790, 500, "ocicfs pull &lt;image@digest&gt;  (optionally delta against a held image)", BLUE, 13, anchor="start")
    # containerd -> cfs
    s.arrow([(910, 201), (986, 201)], BLUE, "ab", both=True)
    s.label(948, 190, "gRPC", BLUE, 12)
    # content store -> differ (data)
    s.arrow([(894, 301), (986, 301)], PURPLE, "ap", dashed=True)
    # differ -> ocicfs (materialize)
    s.arrow([(1244, 301), (1336, 301)], GREEN, "ag")
    s.label(1288, 290, "ingest", GREEN, 12)

    # registry -> content store (path A)
    s.arrow([(760, 660), (760, 540), (760, 450)], PURPLE, "ap", dashed=True)
    s.badge(760, 600, "A", PURPLE)
    s.label(790, 583, "runtime pull: manifest, config,", PURPLE, 13, anchor="start")
    s.label(790, 601, "all missing layer blobs", PURPLE, 13, anchor="start")
    # registry -> ocicfs (path B)
    s.arrow([(910, 705), (1470, 705), (1470, 540)], PURPLE, "ap", dashed=True)
    s.badge(1180, 705, "B", PURPLE)
    s.label(1200, 679, "preload: manifest, config, only the files", PURPLE, 13, anchor="start")
    s.label(1200, 697, "not already in the CAS", PURPLE, 13, anchor="start")

    # notes
    s.note(40, 600, 540, 160, [
        "A  Runtime-driven: the runtime downloads the image; the proxy differ",
        "    applies each layer blob into the shared CAS and ocicfs builds the",
        "    composefs image. Native docker pull progress is preserved.",
        "B  Preloaded: ocicfs pull fetched the image earlier; it creates no runtime",
        "    image records. A later runtime pull registers the image: it fetches",
        "    manifest and config, then the snapshotter reports the chain as already",
        "    present, so no layer blob is downloaded or unpacked.",
    ])

    s.legend(800, [
        (BLUE, "ab", False, "control / API call"),
        (PURPLE, "ap", True, "OCI data (manifests, configs, blobs)"),
        (GREEN, "ag", False, "materialization into the store"),
    ])
    s.write(path)


def run_diagram(path):
    s = SVG("Container Run and Rootfs Assembly", "cfs-snapshotter + ocicfs · how a container gets its root filesystem")

    # Clients
    s.box(40, 120, 270, 380, "client", "Clients")
    s.sbox(56, 166, 238, 70, "client", "Docker client", ["docker run, docker start"])
    s.sbox(56, 256, 238, 90, "client", "composectl / Compose", ["composectl run drives", "docker compose up"])
    s.sbox(56, 366, 238, 70, "client", "containerd client", ["ctr run, nerdctl run"])

    # dockerd
    s.box(370, 166, 180, 70, "docker", "dockerd", "Docker Engine")

    # containerd
    s.box(610, 120, 290, 380, "ctrd", "containerd")
    s.sbox(626, 166, 258, 70, "ctrd", "Container lifecycle", ["create, start, stop, delete"])
    s.sbox(626, 256, 258, 90, "ctrd", "Snapshot request", ["Prepare(key, parent=image chain)", "to the proxy snapshotter"])
    s.sbox(626, 366, 258, 110, "ctrd", "Mount + run", ["performs the returned overlay", "mount, then starts the task", "through the OCI runtime"])

    # cfs-snapshotter
    s.box(960, 120, 290, 380, "cfs", "cfs-snapshotter")
    s.sbox(976, 166, 258, 90, "cfs", "1 · Image mount", ["mounts EROFS metadata read-only;", "one mount, shared per image"])
    s.sbox(976, 276, 258, 90, "cfs", "2 · Writable state", ["creates upperdir + workdir", "for this container"])
    s.sbox(976, 386, 258, 90, "cfs", "3 · Mount spec", ["returns one overlay mount spec;", "does not execute containers"])

    # ocicfs store
    s.box(1310, 120, 250, 250, "ocicfs", "ocicfs store")
    s.sbox(1326, 166, 218, 90, "ocicfs", "Composefs image", ["EROFS metadata per image;", "names each file by digest"])
    s.sbox(1326, 276, 218, 80, "ocicfs", "Shared file CAS", ["file contents, fs-verity sealed,", "shared by every image"])

    # Container rootfs (one overlay)
    s.box(960, 560, 600, 210, "fs", "Container rootfs", "one OverlayFS mount per container, assembled from three directories")
    s.parts.append(f'<rect x="976" y="616" width="568" height="44" rx="6" fill="#fde7ec" stroke="#e06b84" stroke-width="1.5"/>')
    s.text(990, 643, "upperdir + workdir", 15, INK, weight=700)
    s.text(1170, 643, "writable, private to this container (container changes)", 14, MUTED)
    s.parts.append(f'<rect x="976" y="666" width="568" height="44" rx="6" fill="#e8f0fb" stroke="#7fa6d9" stroke-width="1.5"/>')
    s.text(990, 693, "lowerdir: EROFS metadata", 15, INK, weight=700)
    s.text(1210, 693, "read-only, shared by all containers of the image", 14, MUTED)
    s.parts.append(f'<rect x="976" y="716" width="568" height="44" rx="6" fill="#eaf7ec" stroke="#8ac79a" stroke-width="1.5"/>')
    s.text(990, 743, ":: data-only lower: CAS objects", 15, INK, weight=700)
    s.text(1240, 743, "shared by all images, verity=require", 14, MUTED)

    # OCI runtime + container
    s.box(610, 560, 290, 90, "ctrd", "OCI runtime", "containerd-shim-runc-v2 → runc")
    s.box(610, 680, 290, 90, "ocicfs", "Running container", "process tree inside the rootfs")

    # control arrows
    s.arrow([(294, 201), (370, 201)], BLUE, "ab")
    s.arrow([(294, 301), (340, 301), (340, 201)], BLUE, "ab")
    s.arrow([(550, 201), (626, 201)], BLUE, "ab")
    s.arrow([(294, 401), (610, 401)], BLUE, "ab")
    s.arrow([(884, 301), (976, 301)], BLUE, "ab", both=True)
    s.label(930, 290, "Prepare", BLUE, 12)
    s.arrow([(755, 500), (755, 560)], BLUE, "ab")
    s.label(770, 535, "start task", BLUE, 12, anchor="start")
    s.arrow([(755, 650), (755, 680)], BLUE, "ab")
    s.arrow([(884, 421), (940, 421), (940, 540), (1000, 540), (1000, 560)], BLUE, "ab")
    s.label(930, 520, "performs the mount", BLUE, 12, anchor="end")

    # backing arrows (green)
    s.arrow([(1310, 211), (1234, 211)], GREEN, "ag")
    s.label(1272, 200, "mounts", GREEN, 12)
    s.arrow([(1250, 431), (1280, 431), (1280, 560)], GREEN, "ag")
    s.label(1292, 500, "upper/work +", GREEN, 12, anchor="start")
    s.label(1292, 518, "mount spec", GREEN, 12, anchor="start")
    s.arrow([(1435, 370), (1435, 560)], GREEN, "ag")
    s.label(1447, 460, "lowers", GREEN, 12, anchor="start")
    s.arrow([(960, 665), (900, 665), (900, 605)], GREEN, "ag")
    s.label(905, 630, "rootfs", GREEN, 12, anchor="start")

    # note
    s.note(40, 560, 540, 110, [
        "No registry access at run time: every file read is served from the",
        "local CAS through the overlay. The image is never unpacked into a",
        "per-container directory tree, so starting a container costs one mount.",
        "containerd and the OCI runtime execute the container; cfs-snapshotter",
        "only prepares and tears down its filesystem.",
    ])

    s.legend(800, [
        (BLUE, "ab", False, "control / API call"),
        (GREEN, "ag", False, "filesystem backing / mount"),
    ])
    s.write(path)


if __name__ == "__main__":
    out = sys.argv[1]
    pull_diagram(f"{out}/image-pull.svg")
    run_diagram(f"{out}/container-run.svg")
