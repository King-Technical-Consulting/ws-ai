# OCR service for ws document reading (PLAN.md M13 cut 4). Tesseract and
# poppler behind a tiny HTTP server (server.py); Python lives only in here,
# as a black box. Multi-arch: every package is in Debian's arm64 archive.
#
#   docker build -t ws-ocr:latest infra/ocr
#   docker run -d --name ws-ocr -p 127.0.0.1:8000:8000 ws-ocr:latest
#   OCR_URL=http://127.0.0.1:8000        # on ws, app and worker
#
# More languages: --build-arg LANGS="eng deu fra" and OCR_LANGS=eng+deu.
FROM python:3.12-slim-bookworm
ARG LANGS="eng"
RUN apt-get update && apt-get install -y --no-install-recommends tesseract-ocr tesseract-ocr-osd poppler-utils \
    $(for l in $LANGS; do echo tesseract-ocr-$l; done) \
    && rm -rf /var/lib/apt/lists/*
# Pillow turns a sideways page upright before it is read (server.py).
RUN pip install --no-cache-dir "pillow>=10,<12"
WORKDIR /app
COPY server.py /app/server.py
RUN useradd --system --no-create-home ocr
USER ocr
EXPOSE 8000
HEALTHCHECK --interval=30s --timeout=5s CMD python -c "import urllib.request;urllib.request.urlopen('http://127.0.0.1:8000/health',timeout=3)"
CMD ["python", "/app/server.py"]
