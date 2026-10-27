GO ?= go
PREFIX ?= /opt/ttstack
BINDIR ?= bin
GO_PARALLEL ?= 1
export GOMAXPROCS ?= 2
BINS := tt tt-ctl tt-agent upgrade-agent-v5

.PHONY: build release test lint fmt clean install deploy-agent deploy-ctl deploy help cross
build:
	@mkdir -p $(BINDIR)
	@set -e; for name in $(BINS); do $(GO) build -p $(GO_PARALLEL) -o $(BINDIR)/$$name ./cmd/$$name; done

release:
	@mkdir -p $(BINDIR)
	@set -e; for name in $(BINS); do CGO_ENABLED=0 $(GO) build -p $(GO_PARALLEL) -trimpath -ldflags='-s -w' -o $(BINDIR)/$$name ./cmd/$$name; done

test:
	$(GO) test -p $(GO_PARALLEL) ./...

lint:
	@test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }
	$(GO) vet -p $(GO_PARALLEL) ./...

fmt:
	gofmt -w cmd internal api model

cross:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -p $(GO_PARALLEL) ./...
	CGO_ENABLED=0 GOOS=freebsd GOARCH=amd64 $(GO) build -p $(GO_PARALLEL) ./...

clean:
	rm -rf $(BINDIR)

install: release
	install -d $(DESTDIR)$(PREFIX)/bin
	install -m 755 $(addprefix $(BINDIR)/,$(BINS)) $(DESTDIR)$(PREFIX)/bin/

deploy-agent: release
	sudo $(BINDIR)/tt deploy agent --release-dir $(BINDIR)

deploy-ctl: release
	sudo $(BINDIR)/tt deploy ctl --release-dir $(BINDIR)

deploy: release
	sudo $(BINDIR)/tt deploy all --release-dir $(BINDIR)

help:
	@echo 'Targets: build release test lint fmt cross clean install deploy-agent deploy-ctl deploy help'
	@echo 'Deployment requires an authorized Linux target; native acceptance is tracked in docs/roadmap.md.'
