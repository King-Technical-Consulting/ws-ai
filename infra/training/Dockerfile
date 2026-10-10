# Trainer image for ws fine-tune jobs (PLAN.md M10). The worker runs it
# with the dataset at /data, writes the adapter to /out, and reads
# "progress <0..1>" lines from stdout. Python lives only in here, as a
# black box: ws never imports it.
#
#   docker build -t ws-trainer:latest infra/training
#   WS_FINETUNE_IMAGE=ws-trainer:latest   # on the worker
#
# With TRAINER_MODE=serve the same image serves the job over HTTP instead
# (serve.py): that is how a rented GPU runs it. CUDA only (Unsloth). For
# a Mac, run train.py's MLX branch by hand; see README.md.
FROM nvidia/cuda:12.4.1-runtime-ubuntu22.04
ENV DEBIAN_FRONTEND=noninteractive PIP_NO_CACHE_DIR=1 PYTHONUNBUFFERED=1 HF_HOME=/cache/hf
# build-essential: unsloth and trl 1.x use Triton kernels, and Triton needs
# a C compiler and the Python headers at first use (see Dockerfile.orin).
RUN apt-get update && apt-get install -y --no-install-recommends python3 python3-pip python3-dev git build-essential && rm -rf /var/lib/apt/lists/*
RUN pip3 install --upgrade pip && pip3 install "torch==2.5.*" --index-url https://download.pytorch.org/whl/cu124 \
    && pip3 install "unsloth[cu124-torch250]" "trl>=0.12" "datasets>=3" "peft>=0.13" "transformers>=4.46"
# llama.cpp's LoRA converter, so train.py can also write the adapter as a
# GGUF LoRA for llama-server (GGUF_LORA=0 skips it). Pin a commit in prod.
RUN git clone --depth 1 https://github.com/ggml-org/llama.cpp /opt/llama.cpp \
    && pip3 install /opt/llama.cpp/gguf-py
WORKDIR /app
COPY train.py train_mlx.py serve.py entry.sh /app/
RUN chmod +x /app/entry.sh
VOLUME ["/cache/hf"]
EXPOSE 8000
ENTRYPOINT ["/app/entry.sh"]
