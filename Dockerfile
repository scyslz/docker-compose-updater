FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
COPY web ./web
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/compose-auto-updater .

FROM docker:cli
# docker-cli-compose provides the v2 plugin (`docker compose`); the updater also
# auto-detects a legacy `docker-compose` (v1) binary if one is present.
RUN apk add --no-cache docker-cli-compose tzdata
COPY --from=build /out/compose-auto-updater /usr/local/bin/compose-auto-updater
ENV TZ=Asia/Shanghai
ENV CONFIG_FILE=/config/updater.json
VOLUME ["/config"]
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/compose-auto-updater"]
