FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod *.go ./
COPY web ./web
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/openrds .

FROM alpine:3.22 AS disk-helper
RUN apk add --no-cache util-linux e2fsprogs e2fsprogs-extra
# Set the mode explicitly: archives and Windows checkouts can drop the executable bit.
COPY --chmod=0755 disk-helper.sh /usr/local/bin/disk-helper
ENTRYPOINT ["/usr/local/bin/disk-helper"]

# Connection pooling for PostgreSQL. PgBouncer refuses to run as root.
FROM alpine:3.22 AS pooler
RUN apk add --no-cache pgbouncer && adduser -D -H -u 1001 -s /sbin/nologin pgbouncer
USER pgbouncer
CMD ["pgbouncer", "/run/openrds/pgbouncer.ini"]

FROM docker:29-cli AS runtime
LABEL org.opencontainers.image.source="https://github.com/kerjakuyco/openrds" \
      org.opencontainers.image.licenses="Apache-2.0"
# Other servers are reached as Docker over SSH: the console makes its own key
# and reads a new server's host key before trusting it.
RUN apk add --no-cache openssh-keygen openssh-client-common
WORKDIR /opt/openrds
COPY --from=build /out/openrds /usr/local/bin/openrds
COPY engines ./engines
COPY LICENSE NOTICE ./
ENV OPENRDS_DATA_DIR=/var/lib/openrds OPENRDS_LISTEN=0.0.0.0:8080
EXPOSE 8080
ENTRYPOINT ["openrds"]
