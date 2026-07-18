FROM golang:1.24-alpine AS build

WORKDIR /src
COPY go.mod *.go ./
COPY web ./web
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/lightmarks .

FROM scratch
COPY --from=build /out/lightmarks /lightmarks
VOLUME ["/data"]
EXPOSE 5856
ENV PORT=5856 \
    DATA_FILE=/data/bookmarks.json \
    MEMORY_LIMIT_MB=48
ENTRYPOINT ["/lightmarks"]
