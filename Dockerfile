FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/compose-auto-updater .

FROM docker:cli
RUN apk add --no-cache docker-cli-compose tzdata
COPY --from=build /out/compose-auto-updater /usr/local/bin/compose-auto-updater
ENV TZ=Asia/Shanghai
ENTRYPOINT ["/usr/local/bin/compose-auto-updater"]
