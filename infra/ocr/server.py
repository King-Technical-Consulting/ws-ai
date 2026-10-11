"""OCR service for ws document reading (PLAN.md M13 cut 4).

A black box: ws never imports this. It speaks one endpoint.

    POST /ocr   multipart: file (an image or a PDF), max_pages (optional)
    ->  {"pages": [{"n": 1, "text": "..."}]}
    GET  /health -> {"ok": true}

Images go straight to Tesseract; a PDF is rasterised page by page with
pdftoppm (poppler) at 200 dpi first. Each page's orientation is detected
first (Tesseract's OSD pass) and a page that is sideways or upside down is
rotated upright before it is read; a page whose orientation cannot be
detected with confidence is read as it is. Nothing is stored: files are
written to a temporary directory for the length of the request.
"""
import cgi
import json
import os
import subprocess
import tempfile
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import re

LANGS = os.environ.get("OCR_LANGS", "eng")
# Below this OSD confidence the angle is not trusted and the page is read as is.
OSD_MIN_CONFIDENCE = float(os.environ.get("OCR_OSD_MIN_CONFIDENCE", "1.5"))
MAX_BYTES = int(os.environ.get("OCR_MAX_BYTES", str(64 * 1024 * 1024)))
PAGE_TIMEOUT = int(os.environ.get("OCR_PAGE_TIMEOUT", "120"))


def orientation(path: str) -> int:
    """Degrees clockwise the page must turn to be upright: 0, 90, 180 or 270.

    0 when Tesseract's orientation pass fails or is not confident (a page
    with little text, a photograph): reading it as it is is the safe answer.
    """
    try:
        out = subprocess.run(
            ["tesseract", path, "stdout", "--psm", "0", "-l", "osd"],
            capture_output=True, timeout=PAGE_TIMEOUT, check=True,
        ).stdout.decode("utf-8", "replace")
    except (subprocess.CalledProcessError, subprocess.TimeoutExpired):
        return 0
    rot = re.search(r"Rotate:\s*(\d+)", out)
    conf = re.search(r"Orientation confidence:\s*([0-9.]+)", out)
    if not rot or not conf or float(conf.group(1)) < OSD_MIN_CONFIDENCE:
        return 0
    angle = int(rot.group(1)) % 360
    return angle if angle in (90, 180, 270) else 0


def upright(path: str) -> str:
    """Rotate a page image upright in place when it is sideways, and return its path."""
    angle = orientation(path)
    if angle:
        from PIL import Image  # imported late: the service starts without it

        try:
            with Image.open(path) as im:
                fmt = im.format or "PNG"
                # PIL turns counter-clockwise; Tesseract's angle is clockwise.
                rotated = im.rotate(-angle, expand=True)
            # The upload is stored as "in.img": name the format, not the extension.
            rotated.save(path, format=fmt)
        except (OSError, ValueError):
            pass  # unreadable or unwritable: read the page as it is
    return path


def tesseract(path: str) -> str:
    out = subprocess.run(
        ["tesseract", path, "stdout", "-l", LANGS, "--psm", "3"],
        capture_output=True, timeout=PAGE_TIMEOUT, check=True,
    )
    return out.stdout.decode("utf-8", "replace").strip()


def pdf_pages(path: str, workdir: str, max_pages: int) -> list[str]:
    prefix = os.path.join(workdir, "p")
    subprocess.run(
        ["pdftoppm", "-r", "200", "-png", "-l", str(max_pages), path, prefix],
        capture_output=True, timeout=PAGE_TIMEOUT * max(1, max_pages), check=True,
    )
    return sorted(os.path.join(workdir, f) for f in os.listdir(workdir) if f.startswith("p") and f.endswith(".png"))


class Handler(BaseHTTPRequestHandler):
    server_version = "ws-ocr"

    def _json(self, code: int, obj) -> None:
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self) -> None:  # noqa: N802
        if self.path == "/health":
            return self._json(200, {"ok": True})
        self._json(404, {"error": "not found"})

    def do_POST(self) -> None:  # noqa: N802
        if self.path != "/ocr":
            return self._json(404, {"error": "not found"})
        length = int(self.headers.get("Content-Length") or 0)
        if length <= 0 or length > MAX_BYTES:
            return self._json(413, {"error": "file too large"})
        form = cgi.FieldStorage(
            fp=self.rfile, headers=self.headers,
            environ={"REQUEST_METHOD": "POST", "CONTENT_TYPE": self.headers.get("Content-Type", "")},
        )
        if "file" not in form or not getattr(form["file"], "file", None):
            return self._json(400, {"error": "file is required"})
        data = form["file"].file.read()
        mime = form["file"].type or ""
        try:
            max_pages = max(1, min(int(form.getvalue("max_pages") or 50), 500))
        except ValueError:
            max_pages = 50
        try:
            with tempfile.TemporaryDirectory() as tmp:
                src = os.path.join(tmp, "in.pdf" if mime == "application/pdf" or data[:5] == b"%PDF-" else "in.img")
                with open(src, "wb") as f:
                    f.write(data)
                if src.endswith(".pdf"):
                    work = os.path.join(tmp, "pages")
                    os.mkdir(work)
                    images = pdf_pages(src, work, max_pages)
                else:
                    images = [src]
                pages = [{"n": i + 1, "text": tesseract(upright(p))} for i, p in enumerate(images)]
        except subprocess.TimeoutExpired:
            return self._json(504, {"error": "timed out"})
        except subprocess.CalledProcessError:
            return self._json(422, {"error": "could not read the file"})
        self._json(200, {"pages": pages})

    def log_message(self, fmt, *args) -> None:  # request lines only, no file names
        print("ocr:", fmt % args, flush=True)


if __name__ == "__main__":
    port = int(os.environ.get("PORT", "8000"))
    ThreadingHTTPServer(("0.0.0.0", port), Handler).serve_forever()
