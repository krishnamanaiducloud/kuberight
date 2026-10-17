FROM golang:1.23-alpine AS builder
WORKDIR /src
COPY go.mod ./
COPY . .
RUN go mod tidy && go mod verify && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /kuberight ./cmd/kuberight
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /kuberight /kuberight
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/kuberight"]
