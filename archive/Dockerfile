# Everything is downloaded in a stage that runs on the build host, so the arm64 image needs no
# QEMU, and the extensions are baked in so the container never downloads code at runtime.
FROM --platform=$BUILDPLATFORM alpine:3.22 AS fetch
ARG TARGETARCH
ARG DUCKDB=v1.5.5
RUN apk add --no-cache ca-certificates \
 && wget -qO- https://github.com/duckdb/duckdb/releases/download/$DUCKDB/duckdb_cli-linux-$TARGETARCH.gz | gunzip > /duckdb \
 && chmod +x /duckdb \
 && d=/ext/$DUCKDB/linux_$TARGETARCH && mkdir -p $d \
 && for e in ducklake sqlite_scanner httpfs; do \
      wget -qO- http://extensions.duckdb.org/$DUCKDB/linux_$TARGETARCH/$e.duckdb_extension.gz | gunzip > $d/$e.duckdb_extension; \
    done

FROM debian:trixie-slim
COPY --from=fetch /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=fetch /duckdb /usr/local/bin/duckdb
# ponytail: runs as root because DuckDB looks for extensions under $HOME; give it a non-root
# HOME if this container ever listens on a port.
COPY --from=fetch /ext /root/.duckdb/extensions
WORKDIR /archive
COPY lake.sql setup.sql day.sql plane_alert.sql run.sh ./
ENTRYPOINT ["/archive/run.sh"]
