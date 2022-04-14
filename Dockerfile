FROM golang:1.18 AS build
RUN mkdir /app
COPY go.mod /app/
WORKDIR /app
RUN go mod download
COPY . /app
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o rss_sync

FROM scratch

COPY --from=build /app/rss_sync /rss_sync
CMD ["/rss_sync"]