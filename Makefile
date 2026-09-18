.PHONY: build test lint bench bench-overhead bench-enforcement figures clean validate run

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -s -w -X main.version=$(VERSION)

# Load-benchmark mode. quick is the disposable local check, roughly five minutes,
# one repetition at the small payload. evidence is the publishable run, 45 to 60
# minutes, five repetitions across three payload sizes, and it refuses to start on
# a dirty tree or on battery. See benchmarks/README.md.
RESULTS_MODE ?= quick

build:
	go build -ldflags "$(LDFLAGS)" -o levee ./cmd/levee

test:
	go test -race -count=1 ./...

lint:
	golangci-lint run ./...

bench:
	go test -bench=. -benchmem -run=^$$ ./internal/...

# bench-overhead and bench-enforcement generate load and are NOT the CI bench
# target above. Each is one command that produces the raw data and then the
# figure. run.sh prints the results directory as its only stdout line, so the
# path travels between recipe lines through a file rather than a shell variable,
# which make would discard between lines.
bench-overhead:
	RESULTS_MODE=$(RESULTS_MODE) bash benchmarks/harness/run.sh > /tmp/levee-bench-results-dir.txt
	uv run --locked --script benchmarks/plots/overhead_figure.py "$$(tail -1 /tmp/levee-bench-results-dir.txt)"

bench-enforcement:
	RESULTS_MODE=$(RESULTS_MODE) bash benchmarks/harness/run.sh > /tmp/levee-bench-results-dir.txt
	uv run --locked --script benchmarks/plots/enforcement_figure.py "$$(tail -1 /tmp/levee-bench-results-dir.txt)"

# figures re-renders from committed data and generates NO load. This is the
# verification command a stranger runs. RESULTS_DIR is required rather than
# defaulted, because a silently chosen directory would render a figure nobody
# asked for and label it as evidence.
figures:
	@test -n "$(RESULTS_DIR)" || (echo "RESULTS_DIR is required, for example: make figures RESULTS_DIR=benchmarks/results/<dir>" && exit 1)
	uv run --locked --script benchmarks/plots/overhead_figure.py "$(RESULTS_DIR)"
	uv run --locked --script benchmarks/plots/enforcement_figure.py "$(RESULTS_DIR)"

clean:
	rm -f levee coverage.out

validate: build
	./levee validate --config configs/example.yaml

run: build
	./levee serve --config configs/example.yaml
