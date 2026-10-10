VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.1.0-dev)
BIN     := bin/p8sd

.PHONY: build test vet install clean
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(BIN) ./cmd/p8sd
test:
	go test -race ./...
vet:
	gofmt -l . | (! grep .) && go vet ./...
install: build
	install -m 0755 $(BIN) /usr/local/bin/p8sd
	install -m 0644 deploy/p8sd.service /etc/systemd/system/p8sd.service
	install -d -m 0750 /etc/p8s /var/lib/p8s/seeds
	[ -f /etc/p8s/p8sd.json ] || install -m 0600 deploy/p8sd.example.json /etc/p8s/p8sd.json
	systemctl daemon-reload
clean:
	rm -rf bin
