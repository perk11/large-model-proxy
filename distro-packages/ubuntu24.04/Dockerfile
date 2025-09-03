FROM ubuntu:24.04

ARG DEBIAN_FRONTEND=noninteractive
ARG HOST_UID

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

RUN useradd build -u $HOST_UID \
    && mkdir -p /home/build \
    && chown build:build /home/build
