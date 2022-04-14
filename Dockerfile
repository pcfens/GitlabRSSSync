FROM golang:1.18-alpine AS build
RUN mkdir /app
COPY . /app
WORKDIR /app
RUN go mod download

RUN apk add --no-cache git ca-certificates \
    && CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o rss_sync

FROM scratch

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /app/rss_sync /rss_sync

CMD ["/rss_sync"]