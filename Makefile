all: executable build-test-server
test: executable build-test-server
	go test
executable:
	go build -o large-model-proxy main.go
clean:
	go clean
	cd test-server
	go clean
debian-package:
	docker build --build-arg HOST_UID=`id -u` --tag large-model-proxy-ubuntu2204-build distro-packages/ubuntu22.04
	docker run --user build -v .:/opt/src/large-model-proxy large-model-proxy-ubuntu2204-build /opt/src/large-model-proxy/distro-packages/ubuntu22.04/build.sh
build-test-server:
	go build -o test-server/test-server test-server/main.go