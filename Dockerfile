FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
COPY web ./web
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/compose-auto-updater .

# Use plain alpine instead of docker:cli to keep the image small; install only
# the Docker CLI bits we actually need (v2 compose plugin + tzdata for TZ).
FROM alpine:3.20
RUN apk add --no-cache docker-cli docker-cli-compose tzdata
COPY --from=build /out/compose-auto-updater /usr/local/bin/compose-auto-updater
ENV TZ=Asia/Shanghai
ENV CONFIG_FILE=/config/updater.json
VOLUME ["/config"]
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/compose-auto-updater"]
