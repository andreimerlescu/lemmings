BINARY     := lemmings
MODULE     := github.com/andreimerlescu/lemmings
GO         := go
GOFLAGS    := -race
BENCHTIME  ?= 5s
FUZZTIME   ?= 10s
COVEROUT   := coverage.out
DEMO_ADDR  ?= 127.0.0.1:8080

.DEFAULT_GOAL := all

# ── Build ─────────────────────────────────────────────────────────────────────

.PHONY: all
all: lint test build

.PHONY: build
build:
	$(GO) build -trimpath -o bin/$(BINARY) .

.PHONY: build-check
build-check: vet fmt-check
	$(GO) build ./...

.PHONY: install
install:
	$(GO) install .

.PHONY: summary
summary:
	summarize -s useExpanded,templates/lib,.git,.idea,summaries,lemmings -x useExpanded,jpg,LICENSE

# ── Test ──────────────────────────────────────────────────────────────────────

.PHONY: test
test:
	$(GO) test $(GOFLAGS) -count=1 ./...

.PHONY: test-short
test-short:
	$(GO) test $(GOFLAGS) -count=1 -short ./...

.PHONY: test-e2e
test-e2e:
	$(GO) test -count=1 -run '^TestE2E' -v .

FUZZ_TARGETS := FuzzDetectWaitingRoom FuzzSha512sum FuzzExtractSitemapLocs FuzzExtractHTMLLinks \
                FuzzResolveURL FuzzHandleAuth FuzzDashboardHTML FuzzFormatInt FuzzFormatBytes

.PHONY: test-fuzz
test-fuzz:
	@for t in $(FUZZ_TARGETS); do \
		echo "── $$t"; \
		$(GO) test $(GOFLAGS) -run='^$$' -fuzz="^$$t$$" -fuzztime=$(FUZZTIME) . || exit 1; \
	done

.PHONY: test-bench
test-bench:
	$(GO) test -run='^$$' -bench=. -benchmem -benchtime=$(BENCHTIME) ./...

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
	rm -rf bin
	rm -f $(COVEROUT)

# ── Tidy ──────────────────────────────────────────────────────────────────────

.PHONY: tidy
tidy:
	$(GO) mod tidy
	$(GO) mod verify

# ── Run ───────────────────────────────────────────────────────────────────────

# demo starts Neon Arcade, the bundled target with healthy pages, slow and
# flaky pages, soft errors and a waiting room.
.PHONY: demo
demo:
	$(GO) run ./examples/demo -addr $(DEMO_ADDR)

# run points a small swarm at the demo. Start `make demo` in another terminal.
.PHONY: run
run:
	$(GO) run . \
		-hit http://$(DEMO_ADDR)/ \
		-terrain 6 \
		-pack 12 \
		-limit 60 \
		-until 45s \
		-ramp 10s \
		-save-to bin/reports

# run-journey walks the demo's checkout journey with CI gates.
.PHONY: run-journey
run-journey:
	$(GO) run . \
		-hit http://$(DEMO_ADDR)/ \
		-terrain 4 \
		-pack 10 \
		-until 60s \
		-ramp 5s \
		-journey examples/journeys/buy-tokens.json \
		-max-failure-rate 0.05 \
		-p95-budget 750ms \
		-save-to bin/reports

# ── Help ──────────────────────────────────────────────────────────────────────

.PHONY: help
help:
	@echo ""
	@echo "  lemmings — available targets"
	@echo ""
	@echo "  Build"
	@echo "    make all          lint + test + build"
	@echo "    make build        compile bin/lemmings"
	@echo "    make build-check  vet + fmt-check + build (pre-commit gate)"
	@echo "    make install      go install to GOPATH/bin"
	@echo ""
	@echo "  Test"
	@echo "    make test         every test, race detector on, no cache"
	@echo "    make test-short   skip the end-to-end tests"
	@echo "    make test-e2e     only the end-to-end tests (builds the binary)"
	@echo "    make test-fuzz    fuzz every target for FUZZTIME each"
	@echo "    make test-bench   benchmarks"
	@echo "    make test-all     unit + fuzz + bench"
	@echo "    make test-cover   tests with an HTML coverage report"
	@echo ""
	@echo "  Lint"
	@echo "    make lint         go vet + gofmt"
	@echo "    make vet          go vet only"
	@echo "    make fmt          gofmt -w (writes files)"
	@echo "    make fmt-check    gofmt check only (exits 1 if files need formatting)"
	@echo ""
	@echo "  Maintenance"
	@echo "    make clean        remove bin/ and coverage output"
	@echo "    make tidy         go mod tidy + go mod verify"
	@echo ""
	@echo "  Play"
	@echo "    make demo         start the Neon Arcade demo target on $(DEMO_ADDR)"
	@echo "    make run          send 72 lemmings to the demo and open the dashboard"
	@echo "    make run-journey  walk the demo's checkout journey with CI gates"
	@echo ""
	@echo "  Overrides"
	@echo "    FUZZTIME=30s make test-fuzz"
	@echo "    BENCHTIME=10s make test-bench"
	@echo "    DEMO_ADDR=127.0.0.1:9000 make demo run"
	@echo ""
