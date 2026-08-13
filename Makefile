VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -ldflags "-X main.appVersion=$(VERSION) -s -w"

.PHONY: build run clean

build:
	go build $(LDFLAGS) -o freezer .

run:
	go run -ldflags "-X main.appVersion=$(VERSION)" .

clean:
	rm -f freezer freezer.exe
