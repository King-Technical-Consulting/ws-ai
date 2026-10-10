"""LoRA fine-tune for ws on Apple Silicon (PLAN.md M10), with MLX.

The same contract as train.py (environment in, adapter directory out,
"progress <fraction>" lines on stdout), so serve.py can drive it with
TRAINER_SCRIPT=train_mlx.py on a Mac that a ws worker reaches as its
trainer box (WS_FINETUNE_URL). Needs `pip install mlx-lm` and a base
model MLX can load (an mlx-community repo, or any safetensors model it
converts on the fly). Runs `mlx_lm.lora` as a subprocess; nothing here
imports MLX, so the file is importable anywhere.

  BASE_MODEL     Hugging Face id or a local path
  ADAPTER_NAME   name of the adapter (the directory under OUT_DIR)
  TRAIN_FILE     chat-format JSONL: {"messages":[{"role","content"}]}
  EVAL_FILE      optional, same shape (mlx_lm wants a validation set;
                 without one a slice of the training set stands in)
  OUT_DIR        where the adapter goes
  EPOCHS, LEARNING_RATE, LORA_RANK, LORA_ALPHA, MAX_SEQ_LEN
  MLX_BATCH      batch size (default 2); MLX_LAYERS layers to adapt (default 16)

The adapter is MLX's own format (adapters.safetensors and
adapter_config.json), served with `mlx_lm.server --adapter-path`; it is
not a PEFT adapter and llama-server cannot load it.
"""
import json
import math
import os
import re
import shutil
import subprocess
import sys
import tempfile


def env(name, default=None, cast=str):
    v = os.environ.get(name)
    if v is None or v == "":
        if default is None:
            sys.exit(f"{name} is required")
        return default
    return cast(v)


def load_rows(path):
    rows = []
    with open(path) as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            row = json.loads(line)
            flat = []
            for m in row.get("messages") or []:
                role = m.get("role", "user")
                content = m.get("content") or ""
                if m.get("tool_calls"):
                    calls = ", ".join(f'{c["function"]["name"]}({c["function"]["arguments"]})' for c in m["tool_calls"])
                    content = (content + "\n" if content else "") + f"[called {calls}]"
                if role == "tool":
                    role, content = "user", f"[tool result] {content}"
                if not content:
                    continue
                if flat and flat[-1]["role"] == role:
                    flat[-1]["content"] += "\n\n" + content
                else:
                    flat.append({"role": role, "content": content})
            if len(flat) >= 2 and flat[-1]["role"] == "assistant":
                rows.append({"messages": flat})
    return rows


def write_rows(path, rows):
    with open(path, "w") as f:
        for r in rows:
            f.write(json.dumps(r, ensure_ascii=False) + "\n")


def main():
    base = env("BASE_MODEL")
    name = env("ADAPTER_NAME", "adapter")
    train_file = env("TRAIN_FILE", "/data/train.jsonl")
    eval_file = os.environ.get("EVAL_FILE", "/data/eval.jsonl")
    out_dir = env("OUT_DIR", "/out")
    epochs = env("EPOCHS", 2, int)
    lr = env("LEARNING_RATE", 2e-4, float)
    rank = env("LORA_RANK", 16, int)
    alpha = env("LORA_ALPHA", 32, int)
    max_len = env("MAX_SEQ_LEN", 4096, int)
    batch = env("MLX_BATCH", 2, int)
    layers = env("MLX_LAYERS", 16, int)

    train_rows = load_rows(train_file)
    if not train_rows:
        sys.exit("no training examples")
    eval_rows = load_rows(eval_file) if os.path.exists(eval_file) else []
    if not eval_rows:
        # mlx_lm refuses to train without a validation file.
        eval_rows = train_rows[: max(1, min(8, len(train_rows) // 10))]
    print(f"examples train={len(train_rows)} eval={len(eval_rows)} base={base}", flush=True)
    print("progress 0.02", flush=True)

    data = tempfile.mkdtemp(prefix="ws-mlx-")
    write_rows(os.path.join(data, "train.jsonl"), train_rows)
    write_rows(os.path.join(data, "valid.jsonl"), eval_rows)
    target = os.path.join(out_dir, name)
    shutil.rmtree(target, ignore_errors=True)
    os.makedirs(target, exist_ok=True)

    iters = max(1, math.ceil(len(train_rows) / batch) * epochs)
    cfg = {"lora_parameters": {"rank": rank, "scale": alpha / rank, "dropout": 0.0}}
    cfg_path = os.path.join(data, "lora.yaml")
    with open(cfg_path, "w") as f:
        # YAML is a superset of JSON for this shape.
        json.dump(cfg, f)
    cmd = [
        sys.executable, "-m", "mlx_lm", "lora", "--train", "--model", base, "--data", data,
        "--adapter-path", target, "--iters", str(iters), "--batch-size", str(batch),
        "--learning-rate", str(lr), "--num-layers", str(layers), "--max-seq-length", str(max_len),
        "--steps-per-report", "5", "--steps-per-eval", str(max(10, iters // 4)), "--save-every", str(iters),
        "--config", cfg_path,
    ]
    print(" ".join(cmd), flush=True)
    proc = subprocess.Popen(cmd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    iter_re = re.compile(r"^Iter (\d+):")
    for line in proc.stdout:
        line = line.rstrip("\n")
        print(line, flush=True)
        m = iter_re.match(line)
        if m:
            frac = 0.05 + 0.9 * min(1.0, int(m.group(1)) / iters)
            print(f"progress {frac:.3f}", flush=True)
    code = proc.wait()
    shutil.rmtree(data, ignore_errors=True)
    if code != 0:
        sys.exit(f"mlx_lm lora exited with status {code}")
    if not os.path.exists(os.path.join(target, "adapters.safetensors")):
        sys.exit("mlx_lm wrote no adapters.safetensors")
    with open(os.path.join(out_dir, "ws-adapter.json"), "w") as f:
        json.dump({"name": name, "base_model": base, "rank": rank, "alpha": alpha, "epochs": epochs, "examples": len(train_rows), "format": "mlx"}, f)
    print("progress 1.0", flush=True)


if __name__ == "__main__":
    main()
