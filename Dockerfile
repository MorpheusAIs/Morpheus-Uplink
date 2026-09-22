FROM golang:1.22-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=${VERSION}" -o /uplink ./cmd/uplink

FROM alpine:3.20
# Runtime user uid 10001 (CI PR smoke asserts non-root drop via su-exec).
RUN apk add --no-cache ca-certificates su-exec \
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
