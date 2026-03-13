# syntax=docker/dockerfile:1

FROM golang:latest AS builder

WORKDIR /src

COPY go.mod go.sum ./

RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOFLAGS=-mod=readonly go build -trimpath -ldflags="-s -w" -o /out/bugo .

FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    bash \
    ca-certificates \
    curl \
    dbus-x11 \
    file \
    fonts-liberation \
    fonts-noto-cjk \
    fonts-noto-color-emoji \
    git \
    jq \
    less \
    libasound2 \
    libatk-bridge2.0-0 \
    libatk1.0-0 \
    libatspi2.0-0 \
    libcups2 \
    libdbus-1-3 \
    libdrm2 \
    libgbm1 \
    libglib2.0-0 \
    libgtk-3-0 \
    libnspr4 \
    libnss3 \
    libpango-1.0-0 \
    libpangocairo-1.0-0 \
    libx11-6 \
    libx11-xcb1 \
    libxcb1 \
    libxcomposite1 \
    libxdamage1 \
    libxext6 \
    libxfixes3 \
    libxkbcommon0 \
    libxrandr2 \
    libxshmfence1 \
    libxss1 \
    libwayland-client0 \
    libwayland-egl1 \
    nodejs \
    npm \
    openssh-client \
    procps \
    ripgrep \
    tini \
    tzdata \
    unzip \
    x11-utils \
    xauth \
    xdg-utils \
    xvfb \
    xz-utils \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY --from=builder /out/bugo /usr/local/bin/bugo
COPY docker/docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh

ENV BUGO_HOME=/data/.bugo
ENV DISPLAY=:99
ENV BUGO_ENABLE_XVFB=1
ENV XVFB_RESOLUTION=1920x1080x24
ENV XVFB_ARGS=

VOLUME ["/data"]

ENTRYPOINT ["tini", "-g", "--", "/usr/local/bin/docker-entrypoint.sh"]
CMD ["bugo"]
