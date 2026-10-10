# Inference targets

Every box exposes an OpenAI-compatible `/v1/chat/completions` with tool calls and `stream_options.include_usage`. The gateway treats them all the same; `config/endpoints.yaml` declares what each model can do.

| Box | Engine | Compose override | Env var for the gateway |
|---|---|---|---|
| Jetson AGX Orin (arm64 CUDA) | llama-server (CUDA) | `override.orin-cuda.yaml` | `LLAMA_SERVER_URL=http://llama:8000/v1` |
| W7700 (x86 ROCm/Vulkan) | llama-server (Vulkan) | `override.amd-rocm.yaml` | `LLAMA_SERVER_URL=http://llama:8000/v1` |
| DGX Spark / Thor (arm64 CUDA) | vLLM | `override.spark-cuda.yaml` | `VLLM_URL=http://vllm:8000/v1` |
| Any Linux box, CPU only (testing) | llama-server (CPU), Qwen3-4B | `override.cpu.yaml` | `LLAMA_SERVER_URL=http://llama:8000/v1` (the override serves alias `qwen3-4b`; the matching endpoint `local-llama/qwen3-4b` is in `config/endpoints.yaml` but disabled, so turn it on in Admin) |
| Mac Studio (Metal) | Ollama (MLX) or LM Studio headless, native | none; reached over Tailscale | `MAC_STUDIO_URL=http://studio.tailnet:11434/v1` |

## GGUF models for llama-server

Put files in `data/models/`. Suggested starting set:

```bash
pip install -U "huggingface_hub[cli]"
huggingface-cli download unsloth/gpt-oss-20b-GGUF gpt-oss-20b-Q4_K_M.gguf --local-dir data/models
huggingface-cli download unsloth/Qwen3-14B-GGUF Qwen3-14B-Q4_K_M.gguf --local-dir data/models
huggingface-cli download nomic-ai/nomic-embed-text-v1.5-GGUF nomic-embed-text-v1.5.Q8_0.gguf --local-dir data/models
```

The embedding server (`llama-embed`, Orin override) is behind the `embed` Compose profile: enable it with `WS_DEPLOY_PROFILES=embed`, or `PROFILES=embed` for a one-off `make` run. The Qwen3-14B file above is not referenced by any override; point an override's `--model` and `--alias` at it to use it.

To serve a different model, change `--model` and `--alias` in the override and make sure an endpoint in `config/endpoints.yaml` has `model:` equal to the alias.

## Jetson notes

- Check JetPack: `cat /etc/nv_tegra_release`. JetPack 6.x (L4T r36) has CUDA 12.x and recent Docker; newer releases (R39 was seen) also work with the stack. JetPack 5.x will fight you; upgrade first.
- `nvidia-container-toolkit` may or may not be installed and registered with Docker: on one Orin it was a separate apt install from the Jetson repo plus `nvidia-ctk runtime configure --runtime=docker`. Verify `docker info` lists the `nvidia` runtime, then `docker run --rm --runtime nvidia -e NVIDIA_VISIBLE_DEVICES=all ubuntu ls /dev | grep nv` (or `tegrastats` on the host); the older `nvcr.io/nvidia/l4t-base:r36.2.0` image may not match a newer JetPack.
- `GGML_CUDA_ENABLE_UNIFIED_MEMORY=1` lets llama.cpp use the whole unified pool; without it large models can fail to load.
- For vLLM on Thor use `ghcr.io/nvidia-ai-iot/vllm:latest-jetson-thor`, not the generic NGC image.
- **Measured on one AGX Orin (MAXN) with llama.cpp, as `agx-orin` reported it (board `28e0`; not re-run here).** `gpt-oss-20b` (Q4_K_M) decodes at 33 to 36 tok/s on one stream and 81 to 85 tok/s in total at four streams, `--parallel 4`, 49-token and 1,961-token prompts alike for the single stream. These did not change within noise (about 1.5 tok/s, one run each) with `GGML_CUDA_ENABLE_UNIFIED_MEMORY` set or unset, with the native MXFP4 file (12 GB) in place of the Q4_K_M, or with a llama.cpp built on the box for sm_87 instead of the generic `server-cuda` image; a q8_0 KV cache cost 1 to 3 tok/s. GPU and memory clocks sat at their maximum during decode, so `jetson_clocks` was not tried. Qwen3-30B-A3B (Q4_K_M, 18.6 GB) decoded at about 40 tok/s on one stream and 103 in total at four. At roughly 2.4 GB read per token that is about 85 GB/s of the bus's 204 GB/s with the GPU busy, which points at the kernels (llama.cpp's mixture-of-experts path on this GPU) and not at the clocks, the weights format or the build. Long prompts at four streams stay prefill-bound (first token in 4 to 7 s). The unset unified-memory run loaded both models; the note above about large models failing without it is not contradicted for models of this size, only not reproduced.

## Mac Studio notes

- Docker on macOS cannot use the GPU. Run the server natively under launchd.
- Ollama 0.19+ uses MLX on Apple Silicon and serves OpenAI-compatible on `:11434/v1`.
- Raise the GPU memory cap: `sudo sysctl iogpu.wired_limit_mb=<MB>` (default ~75% of RAM).
- Reach it from the GPU box over Tailscale; the gateway health-checks it and routes around it when the Mac is asleep.
