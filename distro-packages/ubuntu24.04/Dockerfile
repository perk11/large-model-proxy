FROM ubuntu:24.04

ARG DEBIAN_FRONTEND=noninteractive

RUN set -ex \
    && apt-get update \
    && apt-get install -y --no-install-recommends  \
    golang-go \
    fakeroot \
    dpkg-dev \
    git \
    ca-certificates \
    && apt-get clean \
    && rm -rf /tmp/* /var/tmp/*

# Use standard user 1000 to satisfy deb checks
USER 1000:1000

# Create host dir with correct permissions
WORKDIR /host
