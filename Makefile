.PHONY: build test race verify demo fmt vet clean

build:
	go build ./...

test:
	go test ./...

race:
	go test -race ./internal/...

# Full end-to-end run: 12 images x 3 layers, 120 files/layer, 400 random ops per
# image, with 2 ms simulated per-request latency.
verify:
	go run ./cmd/verify -out verify-out -images 12 -layers 3 -files 120 -reads 400 -latency 2ms

demo:
	go run ./cmd/ccflz demo -addr :8099 -images 4 -layers 3 -latency 25ms

fmt:
	gofmt -w cmd internal

vet:
	go vet ./...

clean:
	rm -rf verify-out
