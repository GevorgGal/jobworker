.PHONY: proto-gen certs build-server build-client build test lint

# Regenerate after editing jobworker.proto. Commit the generated files.
# Requires buf: https://buf.build/docs/installation
proto-gen:
	buf generate

certs:
	@if [ -f certs/ca.pem ]; then \
		echo "Certificates already exist. Remove certs/*.pem to regenerate."; \
	else \
		cd certs && bash gen.sh; \
	fi

build-server:
	go build -o bin/server ./cmd/server

build-client:
	go build -o bin/jobctl ./cmd/jobctl

build: build-server build-client

test:
	go test -race ./...

lint:
	golangci-lint run ./...
