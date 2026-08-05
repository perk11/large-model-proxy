all: executable build-test-server

docker: executable-docker build-test-server-docker

test: executable build-test-server
	go test -v -parallel 500 -timeout=15m # Tests have a lot of sleeps in them, not CPU bound

test-docker: executable-docker build-test-server-docker
	docker run --rm -v "./.mod:/go/pkg/mod" -v .:/app -w /app golang:1.26-alpine go test -v -parallel 500 -timeout=15m # Tests have a lot of sleeps in them, not CPU bound

executable:
	go build -o large-model-proxy

executable-linux-docker:
	docker run --rm -v "./.mod:/go/pkg/mod" -v .:/app -w /app golang:1.26-alpine go build -o large-model-proxy

executable-linux:
	env GOOS=linux go build -o large-model-proxy-linux

clean:
	go clean
	cd test-server && go clean

debian-package:
	docker run --privileged --rm tonistiigi/binfmt --install arm64
	docker buildx build --platform linux/amd64,linux/arm64 --tag large-model-proxy-ubuntu2404-build --load distro-packages/ubuntu24.04
	docker run --rm -v .:/host --platform linux/amd64 large-model-proxy-ubuntu2404-build /host/distro-packages/ubuntu24.04/build.sh
	docker run --rm -v .:/host --platform linux/arm64 large-model-proxy-ubuntu2404-build /host/distro-packages/ubuntu24.04/build.sh

build-test-server:
	go build -o test-server/test-server test-server/main.go

build-test-server-docker:
	docker run -e GOOS=linux --rm -v "./.mod:/go/pkg/mod" -v .:/app -w /app golang:1.26-alpine go build -o test-server/test-server test-server/main.go
