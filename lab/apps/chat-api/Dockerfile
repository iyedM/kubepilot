# chat-api image for the KubePilot lab. Same pinned base as lock.sh.
FROM python:3.12-slim@sha256:a6e34c598f2467ed0e9a8d349809fcd8b5c603269512df273a0bb1784edc11b1

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1

WORKDIR /app
COPY requirements.txt .
# --require-hashes: every package must match the lock; --no-deps: the lock
# already lists the full dependency tree, nothing else may be resolved.
RUN pip install --require-hashes --no-deps -r requirements.txt

COPY chat_api ./chat_api

# Unprivileged; the root filesystem is mounted read-only by the manifest.
USER 65532:65532
EXPOSE 8000
ENTRYPOINT ["python", "-m", "chat_api"]
