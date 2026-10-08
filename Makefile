BINARY := git_pruner
# Overridable so `make build/clean BINDIR=...` can target the same directory
# install.sh used; the default is the local dev convention.
BINDIR ?= $(HOME)/shared/bin
TARGET := $(BINDIR)/$(BINARY)

.PHONY: build install test vet clean assets

build: $(TARGET)

$(TARGET): $(wildcard *.go) go.mod
	@mkdir -p $(BINDIR)
	go build -o $(TARGET) .

install: build

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -f $(TARGET)

# Re-records the README's demo GIF and screenshot; needs vhs on PATH.
assets:
	./assets/record.sh
