FROM golang:1.27.3-trixie@sha256:c65ecfe613b539ac74c62a4c3b2c27e8f8a6076384c8f27e8a3c0f3be17cc385 AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGET=./cmd/recall-miner
# Short git commit, reported as miner_version "recall-miner/<commit>". The build context
# excludes .git, so compose passes it in (GIT_COMMIT); "dev" when unset.
ARG GIT_COMMIT=dev
RUN CGO_ENABLED=1 GOOS=linux go build -ldflags "-X recall-miner/internal/handlers.Version=${GIT_COMMIT}" -o /out/app ${TARGET}

FROM debian:trixie-slim

# The slim base has no CA bundle; outbound HTTPS (Recall API registration and
# heartbeats, sidecar SystemCertPool) needs the distribution roots.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Pre-create the certificate mountpoint so a new, empty miner-certs volume is
# initialized owned by the runtime user (the native-TLS certificate is written there).
RUN useradd --system --uid 1000 --create-home --home-dir /home/miner miner \
    && mkdir -p /var/lib/recall-miner/certs \
    && chown -R miner:miner /var/lib/recall-miner

COPY --from=build /out/app /usr/local/bin/app

USER miner
EXPOSE 8001

CMD ["app"]
