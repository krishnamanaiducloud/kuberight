# syntax=docker/dockerfile:1.7

FROM ubuntu/python:3.12-24.04_stable@sha256:ec2498ae556d1dd40e896fc6582797ab2b9f051618b3aaaa48c68bca48ce337a AS runtime-base

FROM ubuntu:24.04@sha256:fe81560990fae86dc4743202b2cb4d3df98d0bcf949173cecef15ce1127ef852 AS runtime-libs
RUN mkdir -p /opt/runtime-libs \
        /opt/runtime-metadata/usr/share/doc/libstdc++6 \
        /opt/runtime-metadata/var/lib/dpkg/status.d && \
    library="$(ldconfig -p | awk '$1 == "libstdc++.so.6" { print $NF; exit }')" && \
    test -n "$library" && \
    cp --dereference "$library" /opt/runtime-libs/libstdc++.so.6 && \
    dpkg-query --show libstdc++6 > /opt/runtime-libs/LIBSTDCPP_SOURCE && \
    dpkg-query --status libstdc++6 > /opt/runtime-metadata/var/lib/dpkg/status.d/libstdc++6 && \
    test -f /usr/share/doc/libstdc++6/copyright && \
    cp /usr/share/doc/libstdc++6/copyright /opt/runtime-metadata/usr/share/doc/libstdc++6/copyright
# The chiselled base records its packages only in chisel's manifest, which image scanners do not
# read, so they cannot match Python, OpenSSL and the other Ubuntu packages against Ubuntu's
# security data. Write dpkg status records for them from that manifest.
COPY --from=runtime-base / /opt/runtime-base/
COPY chisel-dpkg-status.sh /tmp/chisel-dpkg-status.sh
RUN apt-get -o Acquire::Retries=5 update && \
    apt-get -o Acquire::Retries=5 install --yes --no-install-recommends jq zstd && \
    sh /tmp/chisel-dpkg-status.sh /opt/runtime-base /opt/runtime-metadata/var/lib/dpkg/status.d && \
    rm -rf /var/lib/apt/lists/* /opt/runtime-base

FROM python:3.12.16-slim-bookworm@sha256:935c3c5b12357a7e10caa9c1e6ca5692344d97cf229832ef584be2cec9fc03df AS build
WORKDIR /build
COPY requirements.lock ./
RUN python -m pip install --no-cache-dir --require-hashes --only-binary=:all: --target /opt/python -r requirements.lock && \
    PYTHONPATH=/opt/python python -c "import fastapi, joblib, numpy, pydantic, scipy, sklearn, uvicorn"

FROM runtime-base
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
COPY --from=runtime-libs --chown=0:0 /opt/runtime-libs /opt/runtime-libs
COPY --from=runtime-libs --chown=0:0 /opt/runtime-metadata/ /
COPY --chown=584792:584792 service.py __init__.py ./
ENV LD_LIBRARY_PATH=/opt/runtime-libs \
    PYTHONPATH=/opt/python \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    KUBERIGHT_ML_MODEL_DIR=/tmp/kuberight-ml/models
USER 584792:584792
EXPOSE 8081
HEALTHCHECK --interval=30s --timeout=4s --start-period=15s --retries=3 CMD ["/usr/bin/python3", "-c", "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8081/healthz', timeout=3).read()"]
ENTRYPOINT ["/usr/bin/python3", "-m", "uvicorn"]
CMD ["service:app", "--host", "0.0.0.0", "--port", "8081", "--no-access-log"]
