BINARY := git_pruner
# Overridable so `make build/clean BINDIR=...` can target the same directory
# install.sh used; the default is the local dev convention.
BINDIR ?= $(HOME)/shared/bin
TARGET := $(BINDIR)/$(BINARY)

.PHONY: build install test vet clean

build: $(TARGET)

$(TARGET): main.go go.mod
	@mkdir -p $(BINDIR)
	go build -o $(TARGET) .

install: build

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -f $(TARGET)
