# syntax=docker/dockerfile:1.7

FROM python:3.12.15-slim-bookworm@sha256:6a0e06f5ba0b373d051aabd3fffb9b873f27f9a2a3a14bcbe37dee6f0e3bbc70 AS build
WORKDIR /build
COPY requirements.lock ./
RUN python -m pip install --no-cache-dir --require-hashes --only-binary=:all: --target /opt/python -r requirements.lock && \
    PYTHONPATH=/opt/python python -c "import fastapi, joblib, numpy, pydantic, scipy, sklearn, uvicorn"

FROM ubuntu/python:3.12-24.04_stable@sha256:e7f3e3c521b5d63316b49024157c4e41ec30494bf70499d178a5cdcdb6918c40
ARG VERSION=dev
ARG VCS_REF=unknown
ARG BUILD_DATE=unknown
LABEL org.opencontainers.image.title="KubeRight ML" \
      org.opencontainers.image.description="Bounded CPU and memory forecasting service for KubeRight" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${VCS_REF}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.source="https://github.com/krishnamanaiducloud/kuberight" \
      org.opencontainers.image.licenses="Apache-2.0"
USER 0:0
# The base image's Pebble supervisor is not used because this image supplies
# its own entrypoint. Remove it so an unused binary is not attack surface.
RUN ["/usr/bin/python3", "-c", "from pathlib import Path; Path('/usr/bin/pebble').unlink()"]
WORKDIR /app
COPY --from=build --chown=584792:584792 /opt/python /opt/python
COPY --chown=584792:584792 service.py __init__.py ./
ENV PYTHONPATH=/opt/python \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    KUBERIGHT_ML_MODEL_DIR=/tmp/kuberight-ml/models
USER 584792:584792
EXPOSE 8081
HEALTHCHECK --interval=30s --timeout=4s --start-period=15s --retries=3 CMD ["/usr/bin/python3", "-c", "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8081/healthz', timeout=3).read()"]
ENTRYPOINT ["/usr/bin/python3", "-m", "uvicorn"]
CMD ["service:app", "--host", "0.0.0.0", "--port", "8081", "--no-access-log"]
