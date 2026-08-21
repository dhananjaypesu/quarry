GO     ?= go
BINDIR ?= bin
PKGS   := ./...

.PHONY: all build test race cover vet fmt bench shell clean

all: vet race build

build:
	@mkdir -p $(BINDIR)
	$(GO) build -o $(BINDIR)/quarry ./cmd/quarry
	$(GO) build -o $(BINDIR)/quarrybench ./cmd/quarrybench

test:
	$(GO) test $(PKGS) -count=1

# The randomized model test is the one that finds real bugs, and it explores a
# different interleaving of statements on every run, so the race target is
# where the suite earns its keep.
race:
	$(GO) test $(PKGS) -race -count=1 -timeout 600s

# -coverpkg matters here: most of the engine is exercised through the SQL
# entry point in the root package, so per-package coverage would report almost
# nothing for the packages doing the work.
cover:
	$(GO) test $(PKGS) -coverpkg=$(PKGS) -coverprofile=coverage.out -count=1
	$(GO) tool cover -func=coverage.out | tail -1

vet:
	$(GO) vet $(PKGS)

fmt:
	$(GO) fmt $(PKGS)

bench: build
	./$(BINDIR)/quarrybench -n 200000 -q 50000

shell: build
	./$(BINDIR)/quarry

clean:
	rm -rf $(BINDIR) benchdata quarrydata coverage.out
