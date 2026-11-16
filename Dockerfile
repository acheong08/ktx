FROM golang:1-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/brain-server ./cmd/brain-server

FROM alpine:3
RUN apk add --no-cache git openssh-client ca-certificates \
 && adduser -D -u 10001 brain \
 && mkdir -p /data && chown brain:brain /data
COPY --from=build /out/brain-server /usr/local/bin/brain-server
USER brain
ENV BRAIN_WORKDIR=/data/repo BRAIN_LISTEN_ADDR=:8080
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["brain-server"]
