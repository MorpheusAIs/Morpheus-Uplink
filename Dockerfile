FROM golang:1.22-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=${VERSION}" -o /uplink ./cmd/uplink

FROM alpine:3.20
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 uplink
USER uplink
COPY --from=build /uplink /usr/local/bin/uplink
ENV DATA_DIR=/data
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["uplink"]
