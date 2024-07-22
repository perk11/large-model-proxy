all: executable
executable:
	go build -o large-model-proxy main.go
clean:
	go clean
debian-package:
	docker build --build-arg HOST_UID=`id -u` --tag large-model-proxy-ubuntu2204-build distro-packages/ubuntu22.04
	docker run --user build -v .:/opt/src/large-model-proxy large-model-proxy-ubuntu2204-build /opt/src/large-model-proxy/distro-packages/ubuntu22.04/build.sh
