VERSION ?= 0.1.0-dev
LDFLAGS := -s -w -X main.version=$(VERSION)
.PHONY: test build
test:
	go vet ./... && go test ./...
build:
	for a in amd64 arm64; do CGO_ENABLED=0 GOOS=linux GOARCH=$$a go build -trimpath -ldflags "$(LDFLAGS)" -o dist/servatum-linux-$$a ./cmd/servatum; done
