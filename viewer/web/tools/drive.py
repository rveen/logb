#!/usr/bin/env python3
"""Drive the viewer in a real browser and screenshot it.

Charts are the one part of this project that tests cannot check. The Go tests
prove the server sends correct numbers; they cannot prove the browser draws
them. That gap is not theoretical — the binary series encoding shipped with a
12-byte header, which made every numeric chart throw
`start offset of Float64Array should be a multiple of 8` and render blank,
while the Go round-trip test compared every byte and passed. This script is
what caught it.

Usage:

    # Start a viewer, plot some signals, screenshot, shut down.
    python3 drive.py --file ../../../testdata/can-example.logb \\
        --fields EngineSpeed,CoolantTemp,Gear

    # Or drive a viewer you already have running.
    python3 drive.py --url http://127.0.0.1:8080/ --fields EngineSpeed

Screenshots land in --out (default ./shots): one after the signals are plotted,
one after a drag-zoom. Page errors and each pane's tier label are printed, so a
blank chart is reported rather than silently photographed.

Requires: google-chrome (or set --chrome), and the websocket-client Python
package. No browser extension and no npm install.
"""

import argparse
import base64
import json
import os
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import time
import urllib.request

try:
    import websocket  # websocket-client
except ImportError:
    sys.exit("need websocket-client: pip install websocket-client")


def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def wait_for(url, timeout=60, what="service"):
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            urllib.request.urlopen(url, timeout=1)
            return
        except Exception:
            time.sleep(0.2)
    raise SystemExit(f"{what} did not come up at {url} within {timeout}s")


class Chrome:
    """A headless Chrome, driven over the DevTools Protocol."""

    def __init__(self, url, binary, width, height, keep_open=False):
        self.port = free_port()
        self.profile = tempfile.mkdtemp(prefix="logbview-drive-")
        self.keep_open = keep_open
        self.proc = subprocess.Popen(
            [
                binary,
                "--headless=new",
                "--disable-gpu",
                "--no-sandbox",
                f"--remote-debugging-port={self.port}",
                # Without this Chrome answers the websocket handshake with 403.
                # It is scoped to a throwaway profile on loopback.
                "--remote-allow-origins=*",
                f"--window-size={width},{height}",
                f"--user-data-dir={self.profile}",
                url,
            ],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            start_new_session=True,
        )
        wait_for(f"http://127.0.0.1:{self.port}/json/version", what="chrome")

        targets = json.load(urllib.request.urlopen(f"http://127.0.0.1:{self.port}/json"))
        pages = [t for t in targets if t["type"] == "page"]
        if not pages:
            raise SystemExit("chrome exposed no page target")
        self.ws = websocket.create_connection(
            pages[0]["webSocketDebuggerUrl"], max_size=64 * 1024 * 1024, timeout=30
        )
        self.msg_id = 0
        self.send("Page.enable")
        self.send("Runtime.enable")

    def send(self, method, **params):
        self.msg_id += 1
        self.ws.send(json.dumps({"id": self.msg_id, "method": method, "params": params}))
        while True:
            msg = json.loads(self.ws.recv())
            if msg.get("id") == self.msg_id:
                if "error" in msg:
                    raise RuntimeError(f"{method}: {msg['error']}")
                return msg.get("result", {})

    def js(self, expr):
        r = self.send("Runtime.evaluate", expression=expr, returnByValue=True, awaitPromise=True)
        if "exceptionDetails" in r:
            raise RuntimeError(f"page threw: {r['exceptionDetails'].get('text')}")
        return r.get("result", {}).get("value")

    def shot(self, path):
        data = self.send("Page.captureScreenshot", format="png")["data"]
        with open(path, "wb") as f:
            f.write(base64.b64decode(data))
        print(f"  wrote {path}")

    def drag(self, x0, x1, y):
        """A left-button drag across the plot, which is how uPlot zooms."""
        for kind, x in (
            ("mousePressed", x0),
            ("mouseMoved", (x0 + x1) // 2),
            ("mouseMoved", x1),
            ("mouseReleased", x1),
        ):
            self.send(
                "Input.dispatchMouseEvent",
                type=kind, x=x, y=y, button="left", clickCount=1, buttons=1,
            )
            time.sleep(0.06)

    def close(self):
        if self.keep_open:
            try:
                self.ws.close()
            except Exception:
                pass
            return

        # Ask nicely first.
        try:
            self.send("Browser.close")
        except Exception:
            # Expected as often as not: Chrome drops the connection before it
            # answers, so the reply never arrives.
            pass
        try:
            self.ws.close()
        except Exception:
            pass

        # Then make sure. Killing the process group is not enough on its own:
        # the browser re-parents away from the launching session, so killpg
        # reaps the launcher and leaves fifty renderers behind. Matching on the
        # throwaway profile path is precise — no other process can be using it.
        kill_group(self.proc)
        for sig in ("-TERM", "-KILL"):
            if not self._alive():
                break
            subprocess.run(
                ["pkill", sig, "-f", f"--user-data-dir={self.profile}"],
                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
            )
            for _ in range(20):
                if not self._alive():
                    break
                time.sleep(0.25)

        if self._alive():
            print(f"  !! chrome survived shutdown (profile {self.profile})", file=sys.stderr)
        shutil.rmtree(self.profile, ignore_errors=True)

    def _alive(self):
        """Whether any process still holds our profile."""
        r = subprocess.run(
            ["pgrep", "-f", f"--user-data-dir={self.profile}"],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        )
        return r.returncode == 0


def kill_group(proc):
    """Chrome and `go run` both spawn children; kill the whole group."""
    if proc.poll() is not None:
        return
    try:
        os.killpg(os.getpgid(proc.pid), signal.SIGTERM)
        proc.wait(timeout=10)
    except Exception:
        try:
            os.killpg(os.getpgid(proc.pid), signal.SIGKILL)
        except Exception:
            pass


def click_field(c, name):
    clicked = c.js(
        "(() => { const b = [...document.querySelectorAll('.field button, .runs button')]"
        f".find(e => e.textContent.trim().startsWith({name!r}));"
        " if (!b) return false; b.click(); return true; })()"
    )
    print(f"  clicked {name}" if clicked else f"  !! no signal named {name}")
    return clicked


def check_alignment(c):
    """Every pane must put the same axis value at the same pixel.

    Panes stack on a shared x-axis, which is only true if their plot areas line
    up. The first lane implementation drew on a plain canvas edge to edge while
    uPlot inset its plot area for the y-axis, so an event at t=1.0 sat dozens of
    pixels from t=1.0 on the chart above it — close enough to look deliberate.
    A screenshot will not tell you that; this will.
    """
    boxes = json.loads(c.js(
        "JSON.stringify([...document.querySelectorAll('.pane .u-over')]"
        ".map(e => { const r = e.getBoundingClientRect();"
        " return {l: Math.round(r.x), r: Math.round(r.right)}; }))"
    ))
    if len(boxes) < 2:
        return []
    lefts = {b["l"] for b in boxes}
    rights = {b["r"] for b in boxes}
    print(f"  plot areas: left {sorted(lefts)}, right {sorted(rights)}")
    if len(lefts) > 1 or len(rights) > 1:
        return [f"panes are not aligned: lefts {sorted(lefts)}, rights {sorted(rights)}"]
    return []


def check_lanes_drew(c):
    """A lane that draws nothing must not pass for a lane with nothing in it.

    State bands and event lanes paint into uPlot's canvas from a draw hook.
    Every way that can go wrong — a scale with no range, a bad clip rect, the
    wrong coordinate system — produces an empty canvas and no error anywhere:
    no exception, no console warning, and a pane header still confidently
    reporting "7 events". Counting non-transparent pixels is the only signal.
    """
    lanes = json.loads(c.js(
        "JSON.stringify([...document.querySelectorAll('.lane canvas')].map(cv => {"
        " const d = cv.getContext('2d').getImageData(0, 0, cv.width, cv.height).data;"
        " let n = 0; for (let i = 3; i < d.length; i += 4) if (d[i] !== 0) n++;"
        " return n; }))"
    ))
    if not lanes:
        return []
    print(f"  lanes painted: {lanes} pixels")
    if any(n == 0 for n in lanes):
        return [f"a lane drew nothing: painted pixel counts {lanes}"]
    return []


def open_drawer(c):
    """Open the bottom drawer, if it is not already open."""
    return c.js(
        "(() => { if (document.querySelector('.drawer')) return true;"
        " const b = [...document.querySelectorAll('header button')]"
        ".find(e => e.textContent.trim() === 'Inspect');"
        " if (!b) return false; b.click(); return true; })()"
    )


def click_tab(c, name):
    return c.js(
        "(() => { const b = [...document.querySelectorAll('.drawer-head .tab')]"
        f".find(e => e.textContent.trim() === {name!r});"
        " if (!b) return false; b.click(); return true; })()"
    )


def open_frame_map(c):
    """Open the frame map and check it drew the file's layout.

    The strip is positioned in percentages of the file size, so a bad offset
    puts a frame off the end of the bar rather than raising anything. Checking
    the boxes are inside their container is the only way that shows up.
    """
    if not open_drawer(c):
        return ["no Inspect button"]
    if not click_tab(c, "Frame map"):
        return ["no Frame map tab"]
    time.sleep(1.5)

    shape = json.loads(c.js(
        "(() => { const strip = document.querySelector('.fm-strip');"
        " if (!strip) return JSON.stringify({frames: -1});"
        " const s = strip.getBoundingClientRect();"
        " const f = [...strip.querySelectorAll('.fm-frame')].map(e => e.getBoundingClientRect());"
        " const segs = strip.querySelectorAll('.fm-seg').length;"
        " const outside = f.filter(r => r.x < s.x - 1 || r.right > s.right + 1).length;"
        " return JSON.stringify({frames: f.length, segs, outside,"
        "   rows: document.querySelectorAll('table.records tbody tr').length}); })()"
    ))
    if shape["frames"] < 0:
        return ["frame map tab opened but drew no strip"]
    print(f"  frame map: {shape['frames']} frames, {shape['segs']} segments, "
          f"{shape['rows']} table rows")
    if shape["frames"] == 0:
        return ["frame map drew no frames"]
    if shape["segs"] == 0:
        return ["frame map drew no segments"]
    if shape["outside"]:
        return [f"{shape['outside']} frames drawn outside the file strip"]
    return []


def open_records(c):
    """Open the record drawer and report what landed in it.

    The table is checkable in a way a chart is not: it either has rows with the
    right number of columns or it does not, and an absent field shows a dash
    rather than a zero. Worth asserting here rather than trusting a screenshot.
    """
    if not open_drawer(c):
        return ["no Inspect button"]
    if not click_tab(c, "Records"):
        return ["no Records tab"]
    time.sleep(1.5)

    shape = json.loads(c.js(
        "(() => { const t = document.querySelector('table.records');"
        " if (!t) return JSON.stringify({rows: -1});"
        " const head = t.querySelectorAll('thead th').length;"
        " const rows = [...t.querySelectorAll('tbody tr')];"
        " const widths = [...new Set(rows.map(r => r.children.length))];"
        " return JSON.stringify({head, rows: rows.length, widths,"
        "   absent: t.querySelectorAll('td.absent').length}); })()"
    ))
    if shape["rows"] < 0:
        return ["record drawer opened but drew no table"]
    print(f"  records: {shape['rows']} rows, {shape['head']} columns, "
          f"{shape['absent']} absent cells")
    if shape["rows"] == 0:
        return ["record table is empty"]
    if shape["widths"] != [shape["head"]]:
        return [f"rows have {shape['widths']} cells against {shape['head']} headers"]
    return []


def start_viewer(logb_file, keep_cache):
    """Build and start logbview on a free port. Returns (proc, url, bindir)."""
    here = os.path.dirname(os.path.abspath(__file__))
    viewer_dir = os.path.abspath(os.path.join(here, "..", ".."))
    bindir = tempfile.mkdtemp(prefix="logbview-bin-")
    binary = os.path.join(bindir, "logbview")

    print(f"  building logbview from {viewer_dir}")
    # This is a module-based project. Force module mode rather than inheriting
    # a GO111MODULE=off left over from a GOPATH-era environment, which fails
    # here with a confusing "cannot find package .../zstd".
    env = dict(os.environ, GO111MODULE="on")
    build = subprocess.run(
        ["go", "build", "-o", binary, "./cmd/logbview"],
        cwd=viewer_dir, env=env, capture_output=True, text=True,
    )
    if build.returncode != 0:
        sys.exit(f"go build failed:\n{build.stderr.strip()}")

    port = free_port()
    args = [binary, "-addr", f"127.0.0.1:{port}", "-open=false"]
    if not keep_cache:
        # Do not leave a sidecar beside the user's file just because a smoke
        # test looked at it.
        args.append("-nocache")
    args.append(os.path.abspath(logb_file))

    proc = subprocess.Popen(
        args, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True
    )
    url = f"http://127.0.0.1:{port}/"
    wait_for(url, what="logbview")
    print(f"  serving {url}")
    return proc, url, bindir


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    src = ap.add_mutually_exclusive_group(required=True)
    src.add_argument("--file", help="a .logb file to open in a viewer started for this run")
    src.add_argument("--url", help="a viewer that is already running")
    ap.add_argument("--fields", default="", help="comma-separated signal names to plot")
    ap.add_argument("--out", default="shots", help="directory for screenshots")
    ap.add_argument("--prefix", default="view", help="screenshot filename prefix")
    ap.add_argument("--chrome", default="google-chrome", help="chrome binary")
    ap.add_argument("--width", type=int, default=1600)
    ap.add_argument("--height", type=int, default=950)
    ap.add_argument("--settle", type=float, default=1.3, help="seconds to wait after each click")
    ap.add_argument("--no-zoom", action="store_true", help="skip the drag-zoom step")
    ap.add_argument("--records", action="store_true", help="also open and check the record table")
    ap.add_argument("--keep-cache", action="store_true", help="let the viewer write its sidecar index")
    args = ap.parse_args()

    os.makedirs(args.out, exist_ok=True)
    viewer = None
    bindir = None
    chrome = None
    failures = []

    try:
        if args.file:
            viewer, url, bindir = start_viewer(args.file, args.keep_cache)
        else:
            url = args.url

        chrome = Chrome(url, args.chrome, args.width, args.height)
        time.sleep(args.settle)

        for name in [f for f in args.fields.split(",") if f]:
            if not click_field(chrome, name):
                failures.append(f"no signal named {name}")
            time.sleep(args.settle)

        failures.extend(check_alignment(chrome))
        failures.extend(check_lanes_drew(chrome))
        chrome.shot(os.path.join(args.out, f"{args.prefix}-charts.png"))

        if not args.no_zoom:
            box = chrome.js(
                "(() => { const el = document.querySelector('.pane .plot .u-over');"
                " if (!el) return null; const r = el.getBoundingClientRect();"
                " return JSON.stringify({x:r.x, y:r.y, w:r.width, h:r.height}); })()"
            )
            if box:
                b = json.loads(box)
                x0 = int(b["x"] + b["w"] * 0.30)
                x1 = int(b["x"] + b["w"] * 0.45)
                y = int(b["y"] + b["h"] / 2)
                chrome.drag(x0, x1, y)
                print(f"  dragged x {x0} -> {x1} to zoom")
                time.sleep(args.settle)
                chrome.shot(os.path.join(args.out, f"{args.prefix}-zoomed.png"))
            else:
                print("  (no numeric chart on screen; skipping zoom)")

        if args.records:
            failures.extend(open_records(chrome))
            chrome.shot(os.path.join(args.out, f"{args.prefix}-records.png"))
            failures.extend(open_frame_map(chrome))
            chrome.shot(os.path.join(args.out, f"{args.prefix}-frames.png"))

        # A blank chart is the failure mode worth catching, and it shows up
        # here rather than in the image.
        tiers = json.loads(chrome.js(
            "JSON.stringify([...document.querySelectorAll('.pane-note')].map(e => e.textContent.trim()))"
        ))
        print(f"  panes: {tiers}")

        errors = json.loads(chrome.js(
            "JSON.stringify([...document.querySelectorAll('.err, .fatal')].map(e => e.textContent.trim()))"
        ))
        if errors:
            failures.extend(errors)
    finally:
        if chrome:
            chrome.close()
        if viewer:
            kill_group(viewer)
        if bindir:
            shutil.rmtree(bindir, ignore_errors=True)

    if failures:
        print("\nFAILED:")
        for f in failures:
            print(f"  - {f}")
        return 1
    print("\nok")
    return 0


if __name__ == "__main__":
    sys.exit(main())
