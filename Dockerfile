# syntax=docker/dockerfile:1.7

# The dashboard bundle is platform independent and the API is cross-compiled, so both build
# stages run natively on the build platform; only the final stage is per target platform.
FROM --platform=$BUILDPLATFORM node:24.22.2-bookworm-slim@sha256:01d566e8d94d8c619591d78652babfccd3c6992a3dffa9ed5ab39e11cf6c70e9 AS web-build
USER root
WORKDIR /app
RUN chown 1000:1000 /app
USER 1000:1000
COPY --chown=1000:1000 web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY --chown=1000:1000 web/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.27.4-bookworm@sha256:4fa7c968ba93f1635670522dbb01a2c78150443e1385746924566079f36570ea AS go-build
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

# Debug variant: the same binary and dashboard on distroless's debug base,
# which adds a BusyBox shell (/busybox/sh) for troubleshooting, still as a
# non-root user. Build it only on purpose: docker build --target debug.
# The production image below has no shell; use it everywhere else.
FROM gcr.io/distroless/static-debian13:debug-nonroot@sha256:7df9fac2f5d3927260696af33fc2391da26366825838a47337088ed78f854a0c AS debug
ARG VERSION=dev
ARG VCS_REF=unknown
ARG BUILD_DATE=unknown
LABEL org.opencontainers.image.title="KubeRight (debug)"       org.opencontainers.image.description="KubeRight with a BusyBox shell, for troubleshooting only"       org.opencontainers.image.version="${VERSION}"       org.opencontainers.image.revision="${VCS_REF}"       org.opencontainers.image.created="${BUILD_DATE}"       org.opencontainers.image.source="https://github.com/krishnamanaiducloud/kuberight"       org.opencontainers.image.licenses="Apache-2.0"
WORKDIR /app
COPY --from=go-build --chown=65532:65532 /out/kuberight /app/kuberight
COPY --from=web-build --chown=65532:65532 /app/dist /app/web
ENV KUBERIGHT_WEB_ROOT=/app/web
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/app/kuberight"]

FROM gcr.io/distroless/static-debian13:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931 AS production
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
COPY --from=go-build --chown=65532:65532 /out/kuberight /app/kuberight
COPY --from=web-build --chown=65532:65532 /app/dist /app/web
ENV KUBERIGHT_WEB_ROOT=/app/web
USER 65532:65532
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=4s --start-period=10s --retries=3 CMD ["/app/kuberight", "healthcheck"]
ENTRYPOINT ["/app/kuberight"]
