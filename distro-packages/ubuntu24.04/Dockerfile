FROM ubuntu:25.10

ARG DEBIAN_FRONTEND=noninteractive
# Overridden by the Makefile with the version from go.mod
ARG GO_VERSION=1.23.0

RUN set -ex \
    && apt-get update \
    && apt-get install -y --no-install-recommends  \
    ca-certificates \
    curl \
    dpkg-dev \
    fakeroot \
    git \
    xz-utils \
    && curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-$(dpkg --print-architecture).tar.gz" -o /tmp/go.tar.gz \
    && tar -C /usr/local -xzf /tmp/go.tar.gz \
    && apt-get clean \
    && rm -rf /tmp/* /var/tmp/*

ENV PATH=/usr/local/go/bin:${PATH}
# The required toolchain is installed in the image; never download another one at build time
ENV GOTOOLCHAIN=local
# UID 1000 has no passwd entry, so Docker would set HOME to the unwritable /
ENV HOME=/tmp

# Use standard user 1000 to satisfy deb checks
USER 1000:1000

# Create host dir with correct permissions
WORKDIR /host
