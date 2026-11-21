FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod *.go ./
COPY web ./web
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/openrds .

FROM alpine:3.22 AS disk-helper
RUN apk add --no-cache util-linux e2fsprogs e2fsprogs-extra
COPY disk-helper.sh /usr/local/bin/disk-helper
ENTRYPOINT ["/usr/local/bin/disk-helper"]

FROM docker:29-cli AS runtime
LABEL org.opencontainers.image.source="https://github.com/kerjakuyco/openrds" \
      org.opencontainers.image.licenses="MIT"
WORKDIR /opt/openrds
COPY --from=build /out/openrds /usr/local/bin/openrds
COPY engines ./engines
COPY LICENSE ./LICENSE
ENV OPENRDS_DATA_DIR=/var/lib/openrds OPENRDS_LISTEN=0.0.0.0:8080
EXPOSE 8080
ENTRYPOINT ["openrds"]
