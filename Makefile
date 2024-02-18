.PHONY: all clean

GOOS := $(or $(GOOS),$(shell go env GOOS))
GOARCH := $(or $(GOARCH),$(shell go env GOARCH))

VERSION := 1.0
GIT_SHA := $(shell git rev-parse --short HEAD)
BUILD_DATE := $(shell date +"%Y-%m-%d %H:%M:%S %Z")

all:
	GOOS=$(GOOS) GOARCH=$(GOARCH) go build -v -ldflags "-X 'main.version=$(VERSION)' -X 'main.gitCommit=$(GIT_SHA)' -X 'main.buildDate=$(BUILD_DATE)'"

clean:
	rm -f rp