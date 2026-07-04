VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test run clean

build:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '$(LDFLAGS)' -o dist/domovoi-linux-amd64 .
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags '$(LDFLAGS)' -o dist/domovoi-linux-arm64 .

test:
	CGO_ENABLED=0 go test ./...

run:
	DOMOVOI_TOKEN=$${DOMOVOI_TOKEN:-dev-token} go run -ldflags '$(LDFLAGS)' . --listen 127.0.0.1:8811

clean:
	rm -rf dist
