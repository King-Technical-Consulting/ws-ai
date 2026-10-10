"""Serve mode of the ws trainer image: the same train.py, driven over HTTP.

Used on a rented GPU (PLAN.md M9 + M10): the box has no Docker socket ws
could reach and no bind mounts, so the worker talks to this instead. The
contract, which internal/training's RentalRunner speaks:

  GET  /status    {"state": "idle|running|done|failed", "progress": 0..1,
                   "log": "<tail>", "error": "..."}
  POST /train     {"base_model", "adapter_name", "config": {...},
                   "train": "<jsonl>", "eval": "<jsonl>"}  -> 202
                   (409 while a run is going)
  GET  /adapter   the output directory as a gzipped tar, once done (409 before)
  POST /cancel    stop the running trainer

A "/v1" prefix is accepted and ignored, since the rental controller hands
out base URLs ending in /v1. Every request must carry
"Authorization: Bearer $TRAINER_API_KEY" (the rental key the template
expands from $WS_RENTAL_API_KEY, or WS_FINETUNE_KEY for a box of your own
at WS_FINETUNE_URL); the server refuses to start without the key, because
a rented box is reachable through the provider's proxy and a box of your
own answers the whole tailnet. The trainer script (train.py, or
TRAINER_SCRIPT=train_mlx.py on a Mac) runs as a subprocess with the
config as environment, its stdout tailed for "progress <fraction>"
lines. Only the standard library, so it runs outside the image too.
"""
import io
import json
import os
import shutil
import subprocess
import sys
import tarfile
import threading
from collections import deque
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

DATA = os.environ.get("TRAINER_DATA", "/data")
OUT = os.environ.get("TRAINER_OUT", "/out")
# TRAINER_SCRIPT picks the trainer: train.py (Unsloth, CUDA) by default,
# train_mlx.py on a Mac. A bare name is looked up next to this file.
_script = os.environ.get("TRAINER_SCRIPT") or "train.py"
TRAIN_PY = _script if os.path.sep in _script else os.path.join(os.path.dirname(os.path.abspath(__file__)), _script)
LOG_LINES = 2000
MAX_BODY = 2 << 30


class Run:
    """The one job this server runs at a time."""

    def __init__(self):
        self.lock = threading.Lock()
        self.state = "idle"
        self.progress = 0.0
        self.error = ""
        self.log = deque(maxlen=LOG_LINES)
        self.proc = None

    def status(self):
        with self.lock:
            return {"state": self.state, "progress": self.progress, "log": "\n".join(self.log) + ("\n" if self.log else ""), "error": self.error}

    def start(self, req):
        with self.lock:
            if self.state == "running":
                return False
            self.state, self.progress, self.error = "running", 0.0, ""
            self.log.clear()
        os.makedirs(DATA, exist_ok=True)
        shutil.rmtree(OUT, ignore_errors=True)
        os.makedirs(OUT, exist_ok=True)
        with open(os.path.join(DATA, "train.jsonl"), "w") as f:
            f.write(req.get("train") or "")
        eval_path = os.path.join(DATA, "eval.jsonl")
        if req.get("eval"):
            with open(eval_path, "w") as f:
                f.write(req["eval"])
        elif os.path.exists(eval_path):
            os.remove(eval_path)
        cfg = req.get("config") or {}
        env = dict(os.environ)
        env.update({
            "BASE_MODEL": str(req.get("base_model") or ""),
            "ADAPTER_NAME": str(req.get("adapter_name") or "adapter"),
            "TRAIN_FILE": os.path.join(DATA, "train.jsonl"),
            "EVAL_FILE": eval_path,
            "OUT_DIR": OUT,
            "EPOCHS": str(cfg.get("epochs") or 2),
            "LEARNING_RATE": str(cfg.get("learning_rate") or 2e-4),
            "LORA_RANK": str(cfg.get("rank") or 16),
            "LORA_ALPHA": str(cfg.get("alpha") or 2 * int(cfg.get("rank") or 16)),
            "MAX_SEQ_LEN": str(cfg.get("max_seq_len") or 4096),
        })
        env.pop("TRAINER_MODE", None)
        env.pop("TRAINER_API_KEY", None)
        env.pop("TRAINER_SCRIPT", None)
        proc = subprocess.Popen([sys.executable, TRAIN_PY], env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
        with self.lock:
            self.proc = proc
        threading.Thread(target=self._pump, args=(proc,), daemon=True).start()
        return True

    def _pump(self, proc):
        for line in proc.stdout:
            line = line.rstrip("\n")
            with self.lock:
                self.log.append(line)
                if line.startswith("progress "):
                    try:
                        self.progress = max(0.0, min(1.0, float(line.split()[1])))
                    except (IndexError, ValueError):
                        pass
        code = proc.wait()
        with self.lock:
            self.proc = None
            if self.state != "running":
                return  # cancelled
            if code == 0 and any(os.scandir(OUT)):
                self.state, self.progress = "done", 1.0
            else:
                self.state = "failed"
                tail = [l for l in list(self.log)[-20:] if l.strip()]
                self.error = f"the trainer exited with status {code}" + (": " + tail[-1] if tail else "")

    def cancel(self):
        with self.lock:
            proc = self.proc
            if self.state == "running":
                self.state, self.error = "idle", "cancelled"
        if proc is not None:
            try:
                proc.terminate()
                proc.wait(timeout=30)
            except Exception:
                proc.kill()

    def adapter(self):
        with self.lock:
            if self.state != "done":
                return None
        buf = io.BytesIO()
        with tarfile.open(fileobj=buf, mode="w:gz") as tar:
            for name in sorted(os.listdir(OUT)):
                tar.add(os.path.join(OUT, name), arcname=name)
        return buf.getvalue()


RUN = Run()
KEY = os.environ.get("TRAINER_API_KEY", "")


class Handler(BaseHTTPRequestHandler):
    server_version = "ws-trainer/1"

    def log_message(self, fmt, *args):  # quiet; the trainer's own output is the log
        pass

    def _path(self):
        p = self.path.split("?", 1)[0]
        if p.startswith("/v1/"):
            p = p[3:]
        return p.rstrip("/") or "/"

    def _auth(self):
        if self.headers.get("Authorization", "") != "Bearer " + KEY:
            self._json(401, {"error": "unauthorized"})
            return False
        return True

    def _json(self, code, obj):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if not self._auth():
            return
        p = self._path()
        if p == "/status":
            self._json(200, RUN.status())
        elif p == "/adapter":
            b = RUN.adapter()
            if b is None:
                self._json(409, {"error": "no adapter: the run is " + RUN.status()["state"]})
                return
            self.send_response(200)
            self.send_header("Content-Type", "application/gzip")
            self.send_header("Content-Length", str(len(b)))
            self.end_headers()
            self.wfile.write(b)
        else:
            self._json(404, {"error": "not found"})

    def do_POST(self):
        if not self._auth():
            return
        p = self._path()
        if p == "/train":
            n = int(self.headers.get("Content-Length") or 0)
            if n <= 0 or n > MAX_BODY:
                self._json(400, {"error": "body required"})
                return
            try:
                req = json.loads(self.rfile.read(n))
            except ValueError:
                self._json(400, {"error": "bad json"})
                return
            if not req.get("base_model") or not req.get("train"):
                self._json(400, {"error": "base_model and train are required"})
                return
            if not RUN.start(req):
                self._json(409, {"error": "a run is in progress"})
                return
            self._json(202, {"status": "started"})
        elif p == "/cancel":
            RUN.cancel()
            self._json(200, {"status": "cancelled"})
        else:
            self._json(404, {"error": "not found"})


def main():
    if not KEY:
        sys.exit("TRAINER_API_KEY is required in serve mode (the box is reachable through the provider's proxy)")
    port = int(os.environ.get("TRAINER_PORT") or 8000)
    srv = ThreadingHTTPServer(("0.0.0.0", port), Handler)
    print(f"ws trainer serving on :{port}", flush=True)
    srv.serve_forever()


if __name__ == "__main__":
    main()
