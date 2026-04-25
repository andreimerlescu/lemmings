BINARY     := lemmings
MODULE     := github.com/andreimerlescu/lemmings
GO         := go
GOFLAGS    := -race
BENCHTIME  := 5s
FUZZTIME   := 10s
COVEROUT   := coverage.out

.DEFAULT_GOAL := all

# ── Build ─────────────────────────────────────────────────────────────────────

.PHONY: all
all: lint test build

.PHONY: build
build:
	$(GO) build -o bin/$(BINARY) .

.PHONY: install
install:
	$(GO) install .

# ── Test ──────────────────────────────────────────────────────────────────────

.PHONY: test
test:
	$(GO) test $(GOFLAGS) ./...

.PHONY: test-fuzz
test-fuzz:
	$(GO) test $(GOFLAGS) -fuzz=FuzzDetectWaitingRoom  -fuzztime=$(FUZZTIME) .
	$(GO) test $(GOFLAGS) -fuzz=FuzzSha512sum          -fuzztime=$(FUZZTIME) .
	$(GO) test $(GOFLAGS) -fuzz=FuzzExtractSitemapLocs -fuzztime=$(FUZZTIME) .
	$(GO) test $(GOFLAGS) -fuzz=FuzzExtractHTMLLinks   -fuzztime=$(FUZZTIME) .
	$(GO) test $(GOFLAGS) -fuzz=FuzzResolveURL         -fuzztime=$(FUZZTIME) .
	$(GO) test $(GOFLAGS) -fuzz=FuzzHandleAuth         -fuzztime=$(FUZZTIME) .
	$(GO) test $(GOFLAGS) -fuzz=FuzzDashboardHTML      -fuzztime=$(FUZZTIME) .
	$(GO) test $(GOFLAGS) -fuzz=FuzzFormatInt          -fuzztime=$(FUZZTIME) .
	$(GO) test $(GOFLAGS) -fuzz=FuzzFormatBytes        -fuzztime=$(FUZZTIME) .

.PHONY: test-bench
test-bench:
	$(GO) test -tags bench -bench=. -benchmem -benchtime=$(BENCHTIME) ./...

.PHONY: test-all
test-all: test test-fuzz test-bench

.PHONY: test-cover
test-cover:
	$(GO) test $(GOFLAGS) -coverprofile=$(COVEROUT) ./...
	$(GO) tool cover -html=$(COVEROUT)

# ── Lint ──────────────────────────────────────────────────────────────────────

.PHONY: lint
lint: vet fmt

.PHONY: vet
vet:
	$(GO) vet ./...

.PHONY: fmt
fmt:
	gofmt -l -w .

.PHONY: fmt-check
fmt-check:
	@if [ -n "$$(gofmt -l .)" ]; then \
		echo "the following files need formatting:"; \
		gofmt -l .; \
		exit 1; \
	fi

# ── Clean ─────────────────────────────────────────────────────────────────────

.PHONY: clean
clean:
	$(GO) clean ./...
	rm -f $(BINARY)
	rm -f $(COVEROUT)

# ── Tidy ──────────────────────────────────────────────────────────────────────

.PHONY: tidy
tidy:
	$(GO) mod tidy
	$(GO) mod verify

# ── Run ───────────────────────────────────────────────────────────────────────

.PHONY: run
run:
	$(GO) run . \
		-hit http://localhost:8080/ \
		-terrain 2 \
		-pack 2 \
		-until 10s \
		-ramp 5s \
		-tty=true

# ── Help ──────────────────────────────────────────────────────────────────────

.PHONY: help
help:
	@echo ""
	@echo "  lemmings — available targets"
	@echo ""
	@echo "  Build"
	@echo "    make all          lint + test + build"
	@echo "    make build        compile the lemmings binary"
	@echo "    make install      go install to GOPATH/bin"
	@echo ""
	@echo "  Test"
	@echo "    make test         unit tests with race detector"
	@echo "    make test-fuzz    fuzz all fuzz targets for FUZZTIME each"
	@echo "    make test-bench   benchmark tests (requires bench build tag)"
	@echo "    make test-all     unit + fuzz + bench"
	@echo "    make test-cover   unit tests with HTML coverage report"
	@echo ""
	@echo "  Lint"
	@echo "    make lint         go vet + gofmt"
	@echo "    make vet          go vet only"
	@echo "    make fmt          gofmt -w (writes files)"
	@echo "    make fmt-check    gofmt check only (exits 1 if files need formatting)"
	@echo ""
	@echo "  Maintenance"
	@echo "    make clean        remove binary and coverage output"
	@echo "    make tidy         go mod tidy + go mod verify"
	@echo ""
	@echo "  Run"
	@echo "    make run          run lemmings with default dev parameters"
	@echo ""
	@echo "  Overrides"
	@echo "    FUZZTIME=30s make test-fuzz"
	@echo "    BENCHTIME=10s make test-bench"
	@echo ""
