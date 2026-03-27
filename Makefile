PROTO_DIR := proto/jobworker/v1

.PHONY: proto-gen certs test lint

# Regenerate after editing jobworker.proto. Commit the generated files.
proto-gen:
	protoc \
		--go_out=. --go_opt=paths=source_relative \
		--go-grpc_out=. --go-grpc_opt=paths=source_relative \
		$(PROTO_DIR)/jobworker.proto

certs:
	cd certs && bash gen.sh

test:
	go test -race ./...

lint:
	golangci-lint run ./...
