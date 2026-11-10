FROM golang:1.27-trixie

ARG LIBDAVE_VERSION=v1.1.0

ENV DEBIAN_FRONTEND=noninteractive \
    CGO_ENABLED=1 \
    PKG_CONFIG_PATH=/root/.local/lib/pkgconfig \
    SHELL=/bin/sh \
    DICPATH=/var/lib/mecab/dic/open-jtalk/naist-jdic \
    VOICEPATH=/opt/voices/tohoku-f01-neutral.htsvoice

WORKDIR /app

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        build-essential \
        ca-certificates \
        cmake \
        curl \
        git \
        libopus-dev \
        make \
        nasm \
        open-jtalk \
        open-jtalk-mecab-naist-jdic \
        pkg-config \
        python3 \
        unzip \
        zip \
    && rm -rf /var/lib/apt/lists/*

RUN mkdir -p /opt/voices /opt/licenses/tohoku-f01 \
    && curl -fsSL -o /opt/voices/tohoku-f01-neutral.htsvoice https://raw.githubusercontent.com/icn-lab/htsvoice-tohoku-f01/master/tohoku-f01-neutral.htsvoice \
    && curl -fsSL -o /opt/licenses/tohoku-f01/COPYRIGHT.txt https://raw.githubusercontent.com/icn-lab/htsvoice-tohoku-f01/master/COPYRIGHT.txt

RUN curl -fsSL -o /tmp/libdave_install.sh https://raw.githubusercontent.com/disgoorg/godave/refs/tags/v0.3.0/scripts/libdave_install.sh \
    && chmod +x /tmp/libdave_install.sh \
    && FORCE_BUILD=1 NON_INTERACTIVE=1 /tmp/libdave_install.sh ${LIBDAVE_VERSION} \
    && rm -f /tmp/libdave_install.sh

COPY go.mod go.sum ./
RUN go mod download

COPY main.go ./
COPY internal ./internal

RUN go build -trimpath -o /usr/local/bin/mtalker .

CMD ["mtalker"]