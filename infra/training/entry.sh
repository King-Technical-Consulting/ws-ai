#!/bin/sh
# Entrypoint of the ws trainer image. TRAINER_MODE=serve runs serve.py
# (a rented GPU the worker drives over HTTP); anything else runs one
# training job from the environment (the worker's Docker runner).
set -e
if [ "${TRAINER_MODE:-train}" = "serve" ]; then
  exec python3 /app/serve.py
fi
exec python3 /app/train.py "$@"
