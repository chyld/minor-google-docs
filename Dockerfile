# syntax=docker/dockerfile:1

# ---- frontend ----
FROM --platform=$BUILDPLATFORM node:26-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN mkdir -p ../internal/webui && npm run build

# ---- backend ----
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS go
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY --from=web /src/internal/webui/dist ./internal/webui/dist
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/notes-bin ./cmd/notes

# ---- runtime ----
FROM alpine:3
RUN apk add --no-cache su-exec tzdata ca-certificates
COPY --from=go /out/notes-bin /usr/local/bin/notes-bin
COPY docker/notes.sh /usr/local/bin/notes
COPY docker/entrypoint.sh /entrypoint.sh
RUN chmod 0755 /usr/local/bin/notes /entrypoint.sh
ENV NOTES_DATA_DIR=/data \
    NOTES_ADDR=:4444 \
    PUID=1000 \
    PGID=1000
VOLUME ["/data"]
# relative paths in CLI commands (export/import) land in the data volume
WORKDIR /data
EXPOSE 4444
ENTRYPOINT ["/entrypoint.sh"]
CMD ["serve"]
