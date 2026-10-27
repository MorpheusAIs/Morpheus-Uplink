FROM golang:1.26.10-alpine@sha256:a2012b4a1d1f9f7e9f44e0d39b805a901e856b1dd80b4e6ee4338db547050308 AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum* ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=${VERSION}" -o /uplink ./cmd/uplink

FROM alpine:3.24@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a
# Runtime user uid 10001 (CI PR smoke asserts non-root drop via su-exec).
# curl: in-container diagnostics (the e2e drives Uplink with it).
RUN apk add --no-cache ca-certificates su-exec curl \
  && adduser -D -u 10001 uplink \
  && mkdir -p /data && chown uplink:uplink /data
COPY --from=build /uplink /usr/local/bin/uplink
COPY deploy/docker-entrypoint.sh /docker-entrypoint.sh
RUN chmod +x /docker-entrypoint.sh
ENV DATA_DIR=/data
VOLUME /data
EXPOSE 8080
# Run entrypoint as root so it can chown the volume, then drop to uplink.
ENTRYPOINT ["/docker-entrypoint.sh"]
