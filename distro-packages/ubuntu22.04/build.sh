#!/bin/bash
set -e

cd /opt/src/large-model-proxy
VERSION=`git describe --tags 2>/dev/null || (echo -n "0.0-dev-" && git rev-parse HEAD)`
ARCH=`dpkg --print-architecture`
BUILD_DIR="package-build/large-model-proxy_${VERSION}_${ARCH}"
TARGET_DIRECTORY='/usr/bin'

make clean
rm -r $BUILD_DIR 2>/dev/null || true
mkdir -p $BUILD_DIR/$TARGET_DIRECTORY
make
cp large-model-proxy $BUILD_DIR/$TARGET_DIRECTORY/
cp -r distro-packages/ubuntu22.04/DEBIAN $BUILD_DIR/

sed -i "s/\$VERSION/$VERSION/g" $BUILD_DIR/DEBIAN/control
dpkg-deb --build --root-owner-group $BUILD_DIR