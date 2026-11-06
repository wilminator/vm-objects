# Copyright (c) 2026 Michael Allen Wilmes
#
# This Source Code Form is subject to the terms of the Mozilla Public
# License, v. 2.0. If a copy of the MPL was not distributed with this
# file, You can obtain one at https://mozilla.org/MPL/2.0/.

VERSION ?= 0.1.0-dev
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build build-all docker-bin docker-image test vet clean

# Static binary for this machine's OS and CPU.
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/vmobj ./cmd/vmobj

# Linux binaries for x86-64 (TrueNAS, most servers) and ARM64.
build-all:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/vmobj-linux-amd64 ./cmd/vmobj
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/vmobj-linux-arm64 ./cmd/vmobj

# The same two binaries, built in a container: needs Docker, not Go.
docker-bin:
	docker build --target export --build-arg VERSION=$(VERSION) --output type=local,dest=bin .

# The runtime image, tagged vmobj:VERSION.
docker-image:
	docker build --build-arg VERSION=$(VERSION) -t vmobj:$(VERSION) .

test:
	go test -race ./...

vet:
	go vet ./...

clean:
	rm -rf bin
