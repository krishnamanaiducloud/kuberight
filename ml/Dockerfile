# syntax=docker/dockerfile:1.7

FROM reg.mini.dev/python:v3.12.15-dev@sha256:2c58651fef1aae3fb24a684b0d5f57190f8cfc73fc3c137be96d6f533c57c53d AS build
USER root
WORKDIR /build
COPY requirements.lock ./
RUN python -m venv /opt/venv && \
    /opt/venv/bin/python -m pip install --no-cache-dir --require-hashes -r requirements.lock && \
    /opt/venv/bin/python -m pip uninstall --yes pip && \
    /opt/venv/bin/python -c "import importlib.util; assert importlib.util.find_spec('pip') is None"

FROM reg.mini.dev/python:v3.12.15@sha256:dccf22b5227855f951150d51f3d0262f5f9e49476ddaedfc7754146686d8979b
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
# The pinned runtime carries an ensurepip bootstrap wheel whose vendored
# libraries are not used by the isolated application venv. Remove that wheel
# from the final filesystem so those unused libraries are not attack surface.
RUN ["/usr/bin/python", "-c", "from pathlib import Path; Path('/usr/share/python-wheels/pip-26.2.1-py3-none-any.whl').unlink()"]
WORKDIR /app
COPY --from=build --chown=1000:1000 /opt/venv /opt/venv
COPY --chown=1000:1000 service.py __init__.py ./
ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    KUBERIGHT_ML_MODEL_DIR=/tmp/kuberight-ml/models
USER 1000:1000
EXPOSE 8081
HEALTHCHECK --interval=30s --timeout=4s --start-period=15s --retries=3 CMD ["/opt/venv/bin/python", "-c", "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8081/healthz', timeout=3).read()"]
ENTRYPOINT ["/opt/venv/bin/python", "-m", "uvicorn"]
CMD ["service:app", "--host", "0.0.0.0", "--port", "8081", "--no-access-log"]
