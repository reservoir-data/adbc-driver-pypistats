GOOS := $(shell go env GOOS)
ifeq ($(GOOS),darwin)
SUFFIX := dylib
else ifeq ($(GOOS),windows)
SUFFIX := dll
else
SUFFIX := so
endif

LIB := build/libadbc_driver_pypistats.$(SUFFIX)

# Only symbols starting with Adbc may be exported from the shared library.
ifeq ($(GOOS),darwin)
EXPORTS := -extldflags=-Wl,-exported_symbols_list,$(CURDIR)/go/pkg/exports.list
else ifeq ($(GOOS),linux)
EXPORTS := -extldflags=-Wl,--version-script=$(CURDIR)/go/pkg/exports.map
endif

.PHONY: build test smoke clean

build: $(LIB)

$(LIB): $(wildcard go/*.go go/pkg/*) go/go.mod go/go.sum
	go build -C go -tags driverlib -buildmode=c-shared -trimpath -ldflags "-s -w $(EXPORTS)" -o ../$(LIB) ./pkg
	rm -f build/libadbc_driver_pypistats.h

test:
	go test -C go -race ./...

# Hits the real pypistats.org API through the ADBC driver manager + profile.
smoke: $(LIB)
	ADBC_PROFILE_PATH=$(CURDIR)/profiles uv run --no-project \
		--with-requirements python/requirements.txt \
		python python/smoke.py $(CURDIR)/$(LIB)

clean:
	rm -rf build
