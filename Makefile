PROTO_DIR := proto/jobworker/v1

.PHONY: proto-gen certs test lint

# Generated with protoc v23.0, protoc-gen-go v1.36.11, protoc-gen-go-grpc v1.6.1
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

# TODO: Add .golangci.yml for reproducible lint results across machines.
lint:
	golangci-lint run ./...
