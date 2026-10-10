"""LoRA fine-tune for ws (PLAN.md M10).

Reads the environment the worker sets:
  BASE_MODEL     Hugging Face id or a path the image can see
  ADAPTER_NAME   name of the adapter (also the directory name under OUT_DIR)
  TRAIN_FILE     chat-format JSONL: {"messages":[{"role","content",...}]}
  EVAL_FILE      optional, same shape (used for eval loss only)
  OUT_DIR        where the adapter goes (/out)
  EPOCHS, LEARNING_RATE, LORA_RANK, LORA_ALPHA, MAX_SEQ_LEN
  HF_TOKEN       optional, for gated models
  TRAINER_BACKEND  auto (default) | unsloth | peft; see load_model
  LOAD_4BIT      1 to load the base in 4-bit on the peft backend (needs bitsandbytes)

Prints "progress <fraction>" lines as training goes; the worker reads
them. Exit status 0 with adapter files in OUT_DIR means success.
"""
import json
import os
import subprocess
import sys


def env(name, default=None, cast=str):
    v = os.environ.get(name)
    if v is None or v == "":
        if default is None:
            sys.exit(f"{name} is required")
        return default
    return cast(v)


def load_jsonl(path):
    rows = []
    with open(path) as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            row = json.loads(line)
            msgs = row.get("messages") or []
            # Tool turns are kept as text so a plain chat template can render them.
            flat = []
            for m in msgs:
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


TARGET_MODULES = ["q_proj", "k_proj", "v_proj", "o_proj", "gate_proj", "up_proj", "down_proj"]


def load_model(base, max_len, rank, alpha):
    """Load the base with LoRA adapters attached, through one of two backends.

    TRAINER_BACKEND: "unsloth" (the fast path on a desktop or data-centre
    GPU, 4-bit base), "peft" (plain transformers + peft, the path for a
    Jetson or any box where unsloth cannot run: no Triton, no bitsandbytes
    needed), or "auto" (default: unsloth when it imports, else peft). The
    backend is printed so the job log says which ran.
    """
    backend = os.environ.get("TRAINER_BACKEND", "auto").lower()
    if backend in ("auto", "unsloth"):
        try:
            from unsloth import FastLanguageModel  # noqa: E402  (slow import, CUDA)
        except ImportError as e:
            if backend == "unsloth":
                sys.exit(f"TRAINER_BACKEND=unsloth but unsloth does not import: {e}")
            print(f"backend: unsloth unavailable ({e.__class__.__name__}), using peft", flush=True)
        else:
            print("backend: unsloth", flush=True)
            model, tokenizer = FastLanguageModel.from_pretrained(model_name=base, max_seq_length=max_len, load_in_4bit=True)
            model = FastLanguageModel.get_peft_model(
                model, r=rank, lora_alpha=alpha, lora_dropout=0, target_modules=TARGET_MODULES,
                use_gradient_checkpointing="unsloth",
            )
            return model, tokenizer
    elif backend != "peft":
        sys.exit(f"TRAINER_BACKEND must be auto, unsloth or peft, not {backend!r}")

    import torch  # noqa: E402
    from peft import LoraConfig, get_peft_model  # noqa: E402
    from transformers import AutoModelForCausalLM, AutoTokenizer  # noqa: E402

    dtype = torch.bfloat16 if torch.cuda.is_available() and torch.cuda.is_bf16_supported() else torch.float32
    device = torch.cuda.get_device_name(0) if torch.cuda.is_available() else "cpu"
    print(f"backend: peft on {device}, dtype {dtype}", flush=True)
    kwargs = {"torch_dtype": dtype, "device_map": "auto" if torch.cuda.is_available() else None}
    if os.environ.get("LOAD_4BIT", "0") == "1":
        # Needs bitsandbytes built for the box; off by default because the
        # Jetson wheels are not on PyPI.
        from transformers import BitsAndBytesConfig  # noqa: E402

        kwargs["quantization_config"] = BitsAndBytesConfig(load_in_4bit=True, bnb_4bit_compute_dtype=dtype, bnb_4bit_quant_type="nf4")
    tokenizer = AutoTokenizer.from_pretrained(base)
    if tokenizer.pad_token is None:
        tokenizer.pad_token = tokenizer.eos_token
    model = AutoModelForCausalLM.from_pretrained(base, **kwargs)
    model.gradient_checkpointing_enable()
    model.enable_input_require_grads()
    model = get_peft_model(model, LoraConfig(r=rank, lora_alpha=alpha, lora_dropout=0, target_modules=TARGET_MODULES, task_type="CAUSAL_LM"))
    model.print_trainable_parameters()
    return model, tokenizer


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

    train_rows = load_jsonl(train_file)
    if not train_rows:
        sys.exit("no training examples")
    eval_rows = load_jsonl(eval_file) if os.path.exists(eval_file) else []
    print(f"examples train={len(train_rows)} eval={len(eval_rows)} base={base}", flush=True)
    print("progress 0.02", flush=True)

    from datasets import Dataset  # noqa: E402
    from trl import SFTConfig, SFTTrainer  # noqa: E402
    from transformers import TrainerCallback  # noqa: E402

    model, tokenizer = load_model(base, max_len, rank, alpha)
    print("progress 0.1", flush=True)

    def render(rows):
        texts = [tokenizer.apply_chat_template(r["messages"], tokenize=False, add_generation_prompt=False) for r in rows]
        return Dataset.from_dict({"text": texts})

    train_ds = render(train_rows)
    eval_ds = render(eval_rows) if eval_rows else None

    class Progress(TrainerCallback):
        def on_log(self, args, state, control, logs=None, **kwargs):
            if state.max_steps:
                frac = 0.1 + 0.85 * (state.global_step / state.max_steps)
                print(f"progress {frac:.3f}", flush=True)
            if logs:
                print(json.dumps(logs), flush=True)

    cfg = dict(
        output_dir="/tmp/run", num_train_epochs=epochs, learning_rate=lr, per_device_train_batch_size=2,
        gradient_accumulation_steps=4, logging_steps=5, save_strategy="no", report_to=[],
        eval_strategy="epoch" if eval_ds is not None else "no", bf16=use_bf16(),
    )
    # trl 1.x renamed SFTConfig.max_seq_length to max_length and SFTTrainer's
    # tokenizer= to processing_class=; take whichever this trl has.
    cfg[pick_kw(SFTConfig, "max_length", "max_seq_length")] = max_len
    trainer = SFTTrainer(
        model=model, train_dataset=train_ds, eval_dataset=eval_ds, args=SFTConfig(**cfg), callbacks=[Progress()],
        **{pick_kw(SFTTrainer, "processing_class", "tokenizer"): tokenizer},
    )
    trainer.train()
    if eval_ds is not None:
        print(json.dumps({"eval": trainer.evaluate()}), flush=True)
    target = os.path.join(out_dir, name)
    os.makedirs(target, exist_ok=True)
    model.save_pretrained(target)
    tokenizer.save_pretrained(target)
    gguf = export_gguf(base, target, name)
    with open(os.path.join(out_dir, "ws-adapter.json"), "w") as f:
        json.dump({"name": name, "base_model": base, "rank": rank, "alpha": alpha, "epochs": epochs, "examples": len(train_rows), "format": "peft", "gguf": gguf}, f)
    print("progress 1.0", flush=True)


def pick_kw(target, *names):
    """The first of names that target (a dataclass or a callable) accepts."""
    import inspect

    try:
        import dataclasses

        if dataclasses.is_dataclass(target):
            have = {f.name for f in dataclasses.fields(target)}
        else:
            have = set(inspect.signature(target).parameters)
    except (TypeError, ValueError):
        return names[-1]
    for n in names:
        if n in have:
            return n
    return names[-1]


def use_bf16():
    """bf16 on a GPU that has it (Ampere and later, the Orin included), else not."""
    try:
        import torch  # noqa: E402

        return bool(torch.cuda.is_available() and torch.cuda.is_bf16_supported())
    except Exception:
        return False


def export_gguf(base, adapter_dir, name):
    """Also write the adapter as a GGUF LoRA (<name>-lora.gguf next to the
    PEFT files) with llama.cpp's converter, for llama-server's --lora.
    Best effort: GGUF_LORA=0 skips it, and a converter that is missing or
    fails leaves the PEFT adapter as the result, with the reason in the
    log. Returns the file name, or None."""
    if os.environ.get("GGUF_LORA", "1") == "0":
        return None
    conv = os.environ.get("LLAMA_CPP_CONVERT", "/opt/llama.cpp/convert_lora_to_gguf.py")
    if not os.path.exists(conv):
        print(f"gguf: converter not found at {conv}; the adapter is PEFT only", flush=True)
        return None
    out = os.path.join(adapter_dir, f"{name}-lora.gguf")
    cmd = [sys.executable, conv, "--base-model-id", base, "--outfile", out, "--outtype", "f16", adapter_dir]
    try:
        res = subprocess.run(cmd, capture_output=True, text=True, timeout=1800)
    except (OSError, subprocess.TimeoutExpired) as e:
        print(f"gguf: conversion failed: {e}", flush=True)
        return None
    if res.returncode != 0 or not os.path.exists(out):
        tail = (res.stderr or res.stdout or "").strip().splitlines()[-3:]
        print("gguf: conversion failed: " + " | ".join(tail), flush=True)
        return None
    print(f"gguf: wrote {os.path.basename(out)}", flush=True)
    return os.path.basename(out)


if __name__ == "__main__":
    main()
