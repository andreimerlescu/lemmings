BINARY     := lemmings
BIN_DIR    := bin
PLATFORMS  := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64
MODULE     := github.com/andreimerlescu/lemmings/v2
CMD_PKG    := ./cmd/lemmings
VERSION    := $(shell cat VERSION)
GO         := go
GOFLAGS    := -race
BENCHTIME  ?= 5s
FUZZTIME   ?= 10s
COVEROUT   := coverage.out
DEMO_ADDR  ?= 127.0.0.1:8080
GO_BUILD_FLAGS := -trimpath
GO_LDFLAGS     := -s -w -X $(MODULE)/internal/version.Build=$(VERSION)

.DEFAULT_GOAL := all

os   = $(word 1,$(subst /, ,$1))
arch = $(word 2,$(subst /, ,$1))
ext  = $(if $(filter windows,$(call os,$1)),.exe,)

# ── Build ─────────────────────────────────────────────────────────────────────

.PHONY: all
all: lint test build

.PHONY: build
build: $(PLATFORMS)

.PHONY: build-local
build-local:
	@mkdir -p $(BIN_DIR)
	$(GO) build $(GO_BUILD_FLAGS) -ldflags "$(GO_LDFLAGS)" -o $(BIN_DIR)/$(BINARY) $(CMD_PKG)

.PHONY: build-check
build-check: vet fmt-check
	$(GO) build ./...

.PHONY: install
install:
	$(GO) install $(GO_BUILD_FLAGS) -ldflags "$(GO_LDFLAGS)" $(CMD_PKG)

# ── Test ──────────────────────────────────────────────────────────────────────

.PHONY: test
test:
	$(GO) test $(GOFLAGS) -count=1 ./...

.PHONY: test-short
test-short:
	$(GO) test $(GOFLAGS) -count=1 -short ./...

.PHONY: test-e2e
test-e2e:
	$(GO) test -count=1 -run '^TestE2E' -v $(CMD_PKG)

.PHONY: test-fuzz
test-fuzz:
	@echo "── discovery fuzz targets"
	@for t in FuzzSHA512Hex FuzzExtractSitemapLocs FuzzExtractHTMLLinks FuzzResolveURL; do \
		echo "   $$t"; \
		$(GO) test $(GOFLAGS) -run='^$$' -fuzz="^$$t$$" -fuzztime=$(FUZZTIME) ./internal/discovery || exit 1; \
	done
	@echo "── secrets fuzz targets"
	@$(GO) test $(GOFLAGS) -run='^$$' -fuzz="^FuzzScrubText$$" -fuzztime=$(FUZZTIME) ./internal/secrets
	@echo "── journey fuzz targets"
	@$(GO) test $(GOFLAGS) -run='^$$' -fuzz="^FuzzExtractErrorBody$$" -fuzztime=$(FUZZTIME) ./internal/journey
	@echo "── swarm fuzz targets"
	@$(GO) test $(GOFLAGS) -run='^$$' -fuzz="^FuzzDetectWaitingRoom$$" -fuzztime=$(FUZZTIME) ./internal/swarm
	@echo "── report fuzz targets"
	@for t in FuzzFormatInt FuzzFormatBytes; do \
		echo "   $$t"; \
		$(GO) test $(GOFLAGS) -run='^$$' -fuzz="^$$t$$" -fuzztime=$(FUZZTIME) ./internal/report || exit 1; \
	done
	@echo "── dashboard fuzz targets"
	@for t in FuzzHandleAuth FuzzDashboardHTML; do \
		echo "   $$t"; \
		$(GO) test $(GOFLAGS) -run='^$$' -fuzz="^$$t$$" -fuzztime=$(FUZZTIME) ./internal/dashboard || exit 1; \
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
	rm -rf $(BIN_DIR)
	rm -f $(COVEROUT)

# ── Tidy ──────────────────────────────────────────────────────────────────────

.PHONY: tidy
tidy:
	$(GO) mod tidy
	$(GO) mod verify

# ── Play ──────────────────────────────────────────────────────────────────────

.PHONY: demo
demo:
	$(GO) run ./examples/demo -addr $(DEMO_ADDR)

.PHONY: run
run:
	$(GO) run $(CMD_PKG) \
		-hit http://$(DEMO_ADDR)/ \
		-terrain 6 \
		-pack 12 \
		-limit 60 \
		-until 45s \
		-ramp 10s \
		-save-to bin/reports

.PHONY: run-forensics
run-forensics:
	$(GO) run $(CMD_PKG) \
		-hit http://$(DEMO_ADDR)/ \
		-terrain 4 \
		-pack 6 \
		-limit 24 \
		-until 30s \
		-ramp 5s \
		-journey examples/journeys/forensics-demo.json \
		-scouts 1 \
		-review \
		-unique-report \
		-save-to bin/reports

.PHONY: run-journey
run-journey:
	$(GO) run $(CMD_PKG) \
		-hit http://$(DEMO_ADDR)/ \
		-terrain 4 \
		-pack 10 \
		-until 60s \
		-ramp 5s \
		-journey examples/journeys/buy-tokens.json \
		-max-failure-rate 0.05 \
		-p95-budget 750ms \
		-save-to bin/reports

$(PLATFORMS):
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 GOOS=$(call os,$@) GOARCH=$(call arch,$@) \
		$(GO) build $(GO_BUILD_FLAGS) -ldflags "$(GO_LDFLAGS)" \
		-o $(BIN_DIR)/$(BINARY)-$(call os,$@)-$(call arch,$@)$(call ext,$@) $(CMD_PKG)

# ── Help ──────────────────────────────────────────────────────────────────────

.PHONY: help
help:
	@echo ""
	@echo "  lemmings $(VERSION) — available targets"
	@echo ""
	@echo "  Build"
	@echo "    make all            lint + test + cross-platform build"
	@echo "    make build-local    compile bin/lemmings for current OS/arch"
	@echo "    make build          compile cross-platform release binaries into bin/"
	@echo "    make build-check    vet + fmt-check + build"
	@echo "    make install        go install ./cmd/lemmings to GOPATH/bin"
	@echo ""
	@echo "  Test"
	@echo "    make test           every test, race detector on, no cache"
	@echo "    make test-short     skip end-to-end tests"
	@echo "    make test-e2e       end-to-end binary tests"
	@echo "    make test-fuzz      fuzz every target for FUZZTIME each"
	@echo "    make test-bench     benchmarks"
	@echo "    make test-all       unit + fuzz + bench"
	@echo "    make test-cover     tests with HTML coverage report"
	@echo ""
	@echo "  Play"
	@echo "    make demo           start Neon Arcade demo target on $(DEMO_ADDR)"
	@echo "    make run            send 72 lemmings to Neon Arcade and open the portal"
	@echo "    make run-forensics  run forensics journey with browser Scout & -review hold"
	@echo "    make run-journey    walk the checkout journey with CI gates"
	@echo ""
