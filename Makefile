.PHONY: test build vet fmt
fmt:
	gofmt -w ./cmd ./internal
test:
	go test ./...
vet:
	go vet ./...
build:
	go build -o bin/kuberight ./cmd/kuberight
