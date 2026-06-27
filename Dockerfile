# YT-DLP Download and setup (per-target-platform, downloads correct binary)
FROM golang:bookworm AS ytdlp_cache
ARG TARGETARCH
RUN apt update && apt install -y wget
RUN case "$TARGETARCH" in \
      amd64)  YT_BIN="yt-dlp_linux" ;; \
      arm64)  YT_BIN="yt-dlp_linux_aarch64" ;; \
      arm)    YT_BIN="yt-dlp_linux_armv7l" ;; \
      *)      YT_BIN="yt-dlp_linux" ;; \
    esac && \
    wget "https://github.com/yt-dlp/yt-dlp/releases/latest/download/${YT_BIN}" -O /usr/local/bin/yt-dlp
RUN chmod a+rx /usr/local/bin/yt-dlp

# Backend setup (cross-compile on build platform, no QEMU needed)
FROM --platform=$BUILDPLATFORM devopsworks/golang-upx:latest AS backend-builder
ARG TARGETOS
ARG TARGETARCH

ENV DEBIAN_FRONTEND=noninteractive
WORKDIR /app
COPY ./backend /app
RUN go mod tidy
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH CGO_ENABLED=0 go build -a -installsuffix cgo -o whishper . && \
    upx whishper
RUN chmod a+rx whishper

# Frontend setup (build once, share across target platforms)
FROM --platform=$BUILDPLATFORM node:20-alpine AS frontend
ENV PNPM_HOME="/pnpm"
ENV PATH="$PNPM_HOME:$PATH"
RUN corepack enable
RUN corepack prepare pnpm@9 --activate
COPY ./frontend /app
WORKDIR /app

FROM frontend AS frontend-prod-deps
RUN --mount=type=cache,id=pnpm,target=/pnpm/store pnpm install --prod --frozen-lockfile

FROM frontend AS frontend-build
RUN --mount=type=cache,id=pnpm,target=/pnpm/store pnpm install --frozen-lockfile
ENV BODY_SIZE_LIMIT=0
RUN pnpm run build

# Base container
FROM python:3.11-slim AS base

RUN export DEBIAN_FRONTEND=noninteractive \
    && apt-get -qq update \
    && apt-get -qq install --no-install-recommends \
    ffmpeg curl nodejs nginx supervisor pkg-config gcc build-essential python3-dev \
    libavformat-dev libavcodec-dev libavdevice-dev libavutil-dev libavfilter-dev libswscale-dev libswresample-dev \
    && rm -rf /var/lib/apt/lists/*

# Python service setup
COPY ./transcription-api /app/transcription
WORKDIR /app/transcription
RUN pip3 install "av>=12,<15" --only-binary=av && \
    pip3 install --no-deps "faster-whisper @ https://github.com/guillaumekln/faster-whisper/archive/e1a218fab1ab02d637b79565995bf1a9c4c83a09.tar.gz" && \
    pip3 install ctranslate2 "huggingface_hub>=0.13" "tokenizers>=0.13,<0.16" "onnxruntime>=1.14,<2" && \
    pip3 install fastapi==0.100.1 pydantic==2.1.1 python-dotenv==1.0.0 uvicorn==0.23.2 ffmpeg-python soundfile
RUN pip3 install python-multipart

# Node.js service setup
ENV BODY_SIZE_LIMIT=0
COPY ./frontend /app/frontend
COPY --from=frontend-build /app/build /app/frontend
COPY --from=frontend-prod-deps /app/node_modules /app/frontend/node_modules

# Golang service setup
COPY --from=backend-builder /app/whishper /bin/whishper 
RUN chmod a+rx /bin/whishper
COPY --from=ytdlp_cache /usr/local/bin/yt-dlp /bin/yt-dlp

# Nginx setup
COPY ./nginx.conf /etc/nginx/nginx.conf

# Set workdir and entrypoint
WORKDIR /app
RUN mkdir /app/uploads

# Cleanup to make the image smaller
RUN apt-get clean && rm -rf /var/lib/apt/lists/* /tmp/* /var/tmp/* /usr/share/doc/* ~/.cache /var/cache

COPY ./supervisord.conf /etc/supervisor/conf.d/supervisord.conf
ENTRYPOINT ["supervisord", "-c", "/etc/supervisor/conf.d/supervisord.conf"]

# Expose ports for each service and Nginx
EXPOSE 8080 3000 5000 80
