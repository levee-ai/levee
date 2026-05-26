.PHONY: build test lint bench clean validate run

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -s -w -X main.version=$(VERSION)

build:
	go build -ldflags "$(LDFLAGS)" -o levee ./cmd/levee

test:
	go test -race -count=1 ./...

lint:
	golangci-lint run ./...

bench:
	go test -bench=. -benchmem -run=^$$ ./internal/...

clean:
	rm -f levee coverage.out

validate: build
	./levee validate --config configs/example.yaml

run: build
	./levee serve --config configs/example.yaml
