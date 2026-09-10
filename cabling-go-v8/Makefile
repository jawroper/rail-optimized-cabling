BINARY := cabling-go
DIST   := dist
VERSION ?= dev
LDFLAGS := -s -w -X main.version=$(VERSION)

PLATFORMS := \
	linux/amd64 \
	linux/arm64 \
	darwin/amd64 \
	darwin/arm64 \
	windows/amd64

.PHONY: all clean $(PLATFORMS) native

all: $(DIST) $(PLATFORMS)

$(DIST):
	mkdir -p $(DIST)

# Build for the current machine (useful for quick local testing).
native:
	go build -o $(BINARY) .

linux/amd64: $(DIST)
	GOOS=linux   GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" \
		-o $(DIST)/$(BINARY)-linux-amd64 .

linux/arm64: $(DIST)
	GOOS=linux   GOARCH=arm64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" \
		-o $(DIST)/$(BINARY)-linux-arm64 .

darwin/amd64: $(DIST)
	GOOS=darwin  GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" \
		-o $(DIST)/$(BINARY)-darwin-amd64 .

darwin/arm64: $(DIST)
	GOOS=darwin  GOARCH=arm64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" \
		-o $(DIST)/$(BINARY)-darwin-arm64 .

windows/amd64: $(DIST)
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" \
		-o $(DIST)/$(BINARY)-windows-amd64.exe .

clean:
	rm -rf $(DIST) $(BINARY)
