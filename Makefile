# Process substitution and pipefail below; /bin/sh on Debian is dash.
SHELL := /bin/bash

.PHONY: test race fuzz vet lint protos check-protos tools

# Where the vendored protos came from. The fork keeps these byte-identical to
# upstream v0.21.3-beta, which is what lets one generated client drive both a
# Lightning Fork node and a stock LND.
PROTO_SOURCE ?= /mnt/Black/lightning-fork
PROTOS := lightning.proto invoices.proto router.proto
MODULE := github.com/paulscode/lightning-fork-bridge

test:
	go test ./...

race:
	go test -race ./...

# Generated code is excluded from the gofmt check: it is protoc's output, not
# ours, and reformatting it would show up as a diff on every regeneration.
vet:
	go vet ./...
	@bad=$$(gofmt -l . | grep -v '^lndrpc/' || true); \
	if [ -n "$$bad" ]; then echo "not gofmt clean:"; echo "$$bad"; exit 1; fi

# The fuzz targets, briefly. They are worth running for much longer before a
# release; these times are what fits in a commit cycle.
fuzz:
	go test -run xxx -fuzz FuzzCheck    -fuzztime 60s ./margin/
	go test -run xxx -fuzz FuzzDepth    -fuzztime 60s ./confirm/
	go test -run xxx -fuzz FuzzOpen     -fuzztime 60s ./store/
	go test -run xxx -fuzz FuzzPreimageHash -fuzztime 30s ./node/

tools:
	go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1

# check-protos fails if the vendored copies have drifted from the fork's.
# Bindings built from a stale proto would misread whatever moved, silently.
check-protos:
	@for f in $(PROTOS); do \
		src=$$(find $(PROTO_SOURCE)/lnrpc -name $$f | head -1); \
		if [ -z "$$src" ]; then \
			echo "$$f: not found under $(PROTO_SOURCE)/lnrpc"; exit 1; \
		fi; \
		if ! diff -q <(grep -v '^option go_package' $$src) \
		             <(grep -v '^option go_package' lndrpc/proto/$$f) \
		             >/dev/null; then \
			echo "$$f has drifted from $$src"; exit 1; \
		fi; \
		echo "$$f matches $$src"; \
	done

protos: check-protos
	protoc -I lndrpc/proto \
		--go_out=. --go_opt=module=$(MODULE) \
		--go-grpc_out=. --go-grpc_opt=module=$(MODULE) \
		$(addprefix lndrpc/proto/,$(PROTOS))
	go build ./...
