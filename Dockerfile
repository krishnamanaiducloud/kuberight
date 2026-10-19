# syntax=docker/dockerfile:1.7

FROM reg.mini.dev/node:v24.22.0-dev@sha256:b41f072456c4ffac8e73d900946e08042acbdc8b0f2eb541dad30561909620e9 AS web-build
USER 1000:1000
WORKDIR /app
COPY --chown=1000:1000 web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY --chown=1000:1000 web/ ./
RUN npm run build

FROM reg.mini.dev/go:v1.27.2-dev@sha256:acd94a6ed1439dbb2b5e6e3303dd5ccce4d6685c33ee7d5cf11edf7cf4f40d70 AS go-build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY cmd/ ./cmd/
COPY internal/ ./internal/

ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=dev
ARG VCS_REF=unknown
ARG BUILD_DATE=unknown
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -mod=readonly -trimpath \
    -ldflags="-s -w -X github.com/kuberight/kuberight/internal/httpapi.Version=${VERSION} -X github.com/kuberight/kuberight/internal/httpapi.Commit=${VCS_REF} -X github.com/kuberight/kuberight/internal/httpapi.BuildDate=${BUILD_DATE}" \
    -o /out/kuberight ./cmd/kuberight

FROM reg.mini.dev/static:latest@sha256:18215a61b935746515939deb207e27182368adeb244a9ae6922e94a21eaac007
ARG VERSION=dev
ARG VCS_REF=unknown
ARG BUILD_DATE=unknown
LABEL org.opencontainers.image.title="KubeRight" \
      org.opencontainers.image.description="Explainable Kubernetes resource optimization control plane" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${VCS_REF}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.source="https://github.com/krishnamanaiducloud/kuberight" \
      org.opencontainers.image.licenses="Apache-2.0"
WORKDIR /app
COPY --from=go-build --chown=1000:1000 /out/kuberight /app/kuberight
COPY --from=web-build --chown=1000:1000 /app/dist /app/web
ENV KUBERIGHT_WEB_ROOT=/app/web
USER 1000:1000
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=4s --start-period=10s --retries=3 CMD ["/app/kuberight", "healthcheck"]
ENTRYPOINT ["/app/kuberight"]
