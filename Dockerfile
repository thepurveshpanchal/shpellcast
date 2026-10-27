# Optional container build. The primary deployment target is a bare
# systemd host (see install.sh) — this exists for people who would rather
# run everything the same way.
#
#   docker build -t shpellcast .
#   docker run -p 8080:8080 -p 1337:1337 \
#     -e SHPELLCAST_BASE_URL=https://shpellcast.godtech.in shpellcast

FROM golang:1.24-alpine AS build
ARG VERSION=docker
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/shpellcast ./cmd/shpellcast

# scratch, not alpine: the binary is static and the web assets are
# embedded in it, so there is nothing else to ship and no shell for an
# attacker to find.
FROM scratch
COPY --from=build /out/shpellcast /shpellcast
EXPOSE 8080 1337
USER 65534:65534
ENTRYPOINT ["/shpellcast"]
