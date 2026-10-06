"""LoRA fine-tune for ws (PLAN.md M10).

Reads the environment the worker sets:
  BASE_MODEL     Hugging Face id or a path the image can see
  ADAPTER_NAME   name of the adapter (also the directory name under OUT_DIR)
  TRAIN_FILE     chat-format JSONL: {"messages":[{"role","content",...}]}
  EVAL_FILE      optional, same shape (used for eval loss only)
  OUT_DIR        where the adapter goes (/out)
  EPOCHS, LEARNING_RATE, LORA_RANK, LORA_ALPHA, MAX_SEQ_LEN
  HF_TOKEN       optional, for gated models

Prints "progress <fraction>" lines as training goes; the worker reads
them. Exit status 0 with adapter files in OUT_DIR means success.
"""
import json
import os
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

    from unsloth import FastLanguageModel  # noqa: E402  (slow import, CUDA)
    from datasets import Dataset  # noqa: E402
    from trl import SFTConfig, SFTTrainer  # noqa: E402
    from transformers import TrainerCallback  # noqa: E402

    model, tokenizer = FastLanguageModel.from_pretrained(model_name=base, max_seq_length=max_len, load_in_4bit=True)
    model = FastLanguageModel.get_peft_model(
        model, r=rank, lora_alpha=alpha, lora_dropout=0,
        target_modules=["q_proj", "k_proj", "v_proj", "o_proj", "gate_proj", "up_proj", "down_proj"],
        use_gradient_checkpointing="unsloth",
    )
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

    trainer = SFTTrainer(
        model=model, tokenizer=tokenizer, train_dataset=train_ds, eval_dataset=eval_ds,
        args=SFTConfig(
            output_dir="/tmp/run", num_train_epochs=epochs, learning_rate=lr, per_device_train_batch_size=2,
            gradient_accumulation_steps=4, logging_steps=5, save_strategy="no", report_to=[],
            eval_strategy="epoch" if eval_ds is not None else "no", max_seq_length=max_len, bf16=True,
        ),
        callbacks=[Progress()],
    )
    trainer.train()
    if eval_ds is not None:
        print(json.dumps({"eval": trainer.evaluate()}), flush=True)
    target = os.path.join(out_dir, name)
    os.makedirs(target, exist_ok=True)
    model.save_pretrained(target)
    tokenizer.save_pretrained(target)
    with open(os.path.join(out_dir, "ws-adapter.json"), "w") as f:
        json.dump({"name": name, "base_model": base, "rank": rank, "alpha": alpha, "epochs": epochs, "examples": len(train_rows)}, f)
    print("progress 1.0", flush=True)


if __name__ == "__main__":
    main()
