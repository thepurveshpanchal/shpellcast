# shpellcast — build, test and release.
#
#   make              build for this machine into bin/
#   make test         vet + race tests
#   make release      static binaries for linux amd64/arm64 + checksums
#   make run          run locally on :8080 / :1337
#   make install      build and install as a systemd service (needs root)

BINARY      := shpellcast
PKG         := ./cmd/shpellcast
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE        ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

# -s -w strips the symbol table and DWARF: smaller binary, faster transfer,
# and nothing useful is lost for a service that logs its own errors.
LDFLAGS     := -s -w \
               -X main.version=$(VERSION) \
               -X main.commit=$(COMMIT) \
               -X main.date=$(DATE)

# CGO off makes the binary genuinely static, so it runs on any glibc or
# musl host — Ubuntu, Alpine, a distroless container, a Raspberry Pi.
GOFLAGS     := -trimpath
export CGO_ENABLED = 0

PLATFORMS   := linux/amd64 linux/arm64 linux/arm darwin/amd64 darwin/arm64

.DEFAULT_GOAL := build
.PHONY: build bench test race vet fmt lint run clean release install uninstall docker tidy check loadtest

build:
	@mkdir -p bin
	go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/$(BINARY) $(PKG)
	@echo "built bin/$(BINARY) $(VERSION) ($$(du -h bin/$(BINARY) | cut -f1))"

bench:
	@mkdir -p bin
	go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/$(BINARY)-bench ./cmd/shpellcast-bench

# Capacity check against a running server. Override with e.g.
#   make loadtest SESSIONS=200 VIEWERS=2 RATE=10 DURATION=60s
SESSIONS ?= 100
VIEWERS  ?= 2
RATE     ?= 5
DURATION ?= 30s
HTTP     ?= http://127.0.0.1:8080
TCP      ?= 127.0.0.1:1337

loadtest: bench
	./bin/$(BINARY)-bench -http $(HTTP) -tcp $(TCP) \
	  -sessions $(SESSIONS) -viewers $(VIEWERS) -rate $(RATE) -duration $(DURATION)

test:
	go test ./...

race:
	# The race detector needs cgo, which the rest of the build deliberately
	# turns off to keep the shipped binary static.
	CGO_ENABLED=1 go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -w cmd internal

check: vet race
	@test -z "$$(gofmt -l cmd internal)" || { echo "unformatted files:"; gofmt -l cmd internal; exit 1; }
	@echo "all checks passed"

run: build
	./bin/$(BINARY) -log-level debug

clean:
	rm -rf bin dist

tidy:
	go mod tidy

release: clean
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; \
	  out=dist/$(BINARY)-$(VERSION)-$$os-$$arch; \
	  echo "  $$os/$$arch"; \
	  GOOS=$$os GOARCH=$$arch go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $$out $(PKG) || exit 1; \
	  gzip -9 -c $$out > $$out.gz; rm $$out; \
	done
	@cd dist && sha256sum *.gz > SHA256SUMS
	@ls -lh dist

install: build
	@test "$$(id -u)" = "0" || { echo "run as root: sudo make install"; exit 1; }
	install -d /usr/local/bin /etc/shpellcast
	install -m 0755 bin/$(BINARY) /usr/local/bin/$(BINARY)
	ln -sf /usr/local/bin/$(BINARY) /usr/local/bin/shpell
	@test -f /etc/shpellcast/shpellcast.env || install -m 0644 deploy/shpellcast.env /etc/shpellcast/shpellcast.env
	install -m 0644 deploy/shpellcast.service /etc/systemd/system/shpellcast.service
	systemctl daemon-reload
	systemctl enable --now shpellcast
	@systemctl --no-pager --lines=5 status shpellcast || true

uninstall:
	@test "$$(id -u)" = "0" || { echo "run as root: sudo make uninstall"; exit 1; }
	-systemctl disable --now shpellcast
	rm -f /etc/systemd/system/shpellcast.service /usr/local/bin/$(BINARY) /usr/local/bin/shpell
	systemctl daemon-reload
	@echo "config left in /etc/shpellcast (remove it by hand if you want it gone)"

docker:
	docker build -t shpellcast:$(VERSION) --build-arg VERSION=$(VERSION) .
