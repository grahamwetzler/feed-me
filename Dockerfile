# syntax=docker/dockerfile:1

FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
# modernc.org/sqlite is pure Go, so the binary is static.
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/rss-er ./cmd/rss-er \
 && mkdir /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/rss-er /rss-er
COPY deploy/rss-er.yaml /config/rss-er.yaml
COPY sites /sites
# A named volume copies this directory's owner, so the nonroot user can write
# the store. A bind-mounted /data must be writable by uid 65532 itself.
COPY --from=build --chown=nonroot:nonroot /out/data /data
ENV RSS_ER_CONFIG=/config/rss-er.yaml
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["/rss-er", "healthcheck"]
ENTRYPOINT ["/rss-er"]
CMD ["run"]
