all: executable build-test-server
test: build-test-server
	go build -tags testhooks -o large-model-proxy
	go test -v -parallel 500 #Tests have a lot of sleeps in them, not CPU bound
executable:
	go build -o large-model-proxy
clean:
	go clean
	cd test-server && go clean
GO_VERSION := $(shell awk '/^go /{v=$$2; n=split(v,a,"."); if(n==2) v=v".0"; print v; exit}' go.mod)
debian-package:
	docker run --privileged --rm tonistiigi/binfmt --install arm64
	docker buildx build --platform linux/amd64,linux/arm64 --build-arg GO_VERSION=$(GO_VERSION) --tag large-model-proxy-ubuntu2404-build --load distro-packages/ubuntu24.04
	docker run --rm -v .:/host --platform linux/amd64 large-model-proxy-ubuntu2404-build /host/distro-packages/ubuntu24.04/build.sh
	docker run --rm -v .:/host --platform linux/arm64 large-model-proxy-ubuntu2404-build /host/distro-packages/ubuntu24.04/build.sh
build-test-server:
	go build -o test-server/test-server test-server/main.go
