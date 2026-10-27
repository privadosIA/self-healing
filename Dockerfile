# syntax=docker/dockerfile:1
#   docker build -t healerd .
# The runtime image is Debian slim and not distroless because healerd shells out to git
# (clone, apply --check, commit, push).

# ─── Build ──────────────────────────────────────────────────
FROM golang:1.27.3-bookworm AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags '-s -w' -o /out/healerd ./cmd/healerd

# ─── Runtime ────────────────────────────────────────────────
FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends git ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --system --uid 65532 --create-home --home-dir /home/healer healer \
    && mkdir /data && chown healer:healer /data

COPY --from=build /out/healerd /usr/local/bin/healerd

USER healer
ENV HEALER_DB_PATH=/data/healerd.db
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 CMD ["/usr/local/bin/healerd", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/healerd"]
