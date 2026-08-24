# The Go half of MCPDoll: three binaries, the fixture backends, and the WASM
# plugins, in one image.
#
# One image rather than four. Every service here is a few megabytes of static
# Go, they share a build cache, and a compose stack that pulls one image starts
# far faster than one that builds four. The entrypoint selects the binary.

# ------------------------------------------------------------------ build ----
FROM golang:1.26-alpine AS build

WORKDIR /src

# Dependencies first, as their own layer. A source edit then rebuilds in
# seconds instead of re-downloading the module graph, which is the difference
# between a stack you rebuild every turn and one you avoid rebuilding.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO off so the binaries are static and the runtime image needs no libc
# beyond what alpine already has. wazero is pure Go, so the WASM host works
# under this too.
ENV CGO_ENABLED=0
RUN go build -trimpath -o /out/bin/mcpdoll        ./cmd/mcpdoll     \
 && go build -trimpath -o /out/bin/mcpdoll-dp     ./cmd/mcpdoll-dp  \
 && go build -trimpath -o /out/bin/mcpdoll-cp     ./cmd/mcpdoll-cp  \
 && go build -trimpath -o /out/bin/fixture-backend ./fixtures/cmd/fixture-backend

# The plugins are reactor modules — `-buildmode=c-shared`. A command module
# would run main and exit, and could not be invoked afterwards. See
# docs/architecture/plugin-authoring.md.
RUN GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared \
      -o /out/plugins/redact.wasm ./plugins/redact \
 && GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared \
      -o /out/plugins/entitlements.wasm ./plugins/entitlements

# ---------------------------------------------------------------- runtime ----
FROM alpine:3.21

# curl for the compose healthchecks. The alternative is probing with bash's
# /dev/tcp, which proves a port accepts connections and nothing about whether
# the process behind it is serving.
RUN apk add --no-cache ca-certificates curl tzdata \
 && adduser -D -u 10001 -h /home/mcpdoll mcpdoll

COPY --from=build /out/bin/ /usr/local/bin/
COPY --from=build /out/plugins/ /srv/plugins/

# The registry document and the data-plane config travel in the image so the
# bootstrap has a source to transform. State the container *writes* goes to a
# volume at /srv/state; nothing here is written at runtime.
COPY deploy/local/registry.yaml   /srv/src/registry.yaml
COPY deploy/docker/dataplane.yaml /srv/src/dataplane.yaml
COPY deploy/docker/bootstrap.sh   /usr/local/bin/bootstrap.sh
COPY deploy/docker/entrypoint-dp.sh /usr/local/bin/entrypoint-dp.sh
COPY deploy/docker/seed.sh        /usr/local/bin/seed.sh
RUN chmod +x /usr/local/bin/bootstrap.sh /usr/local/bin/entrypoint-dp.sh \
      /usr/local/bin/seed.sh

RUN mkdir -p /srv/state && chown -R mcpdoll:mcpdoll /srv/state
USER mcpdoll
WORKDIR /srv

# No default CMD. Each compose service names the binary it wants, which keeps
# "what does this container run" answerable from the compose file rather than
# from an image label.
