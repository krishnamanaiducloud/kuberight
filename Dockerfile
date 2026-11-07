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

FROM --platform=$BUILDPLATFORM golang:1.27.3-bookworm@sha256:dc0963f32053e4bc451d24d87e3892d42d01021b61f093e98eb95c0352bb84b0 AS go-build
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

FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329
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
