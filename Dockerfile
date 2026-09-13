# syntax=docker/dockerfile:1

# ---- 构建阶段 ----
FROM golang:1.23-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/sysmonitor-exporter .

# ---- 运行阶段：ubuntu 24.04，内置 wget / curl ----
FROM ubuntu:24.04

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        tzdata \
        wget \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system exporter \
    && useradd --system --gid exporter --shell /usr/sbin/nologin exporter

COPY --from=build /out/sysmonitor-exporter /usr/local/bin/sysmonitor-exporter
COPY config.yaml /etc/sysmonitor-exporter/config.yaml

EXPOSE 9108
USER exporter

ENTRYPOINT ["sysmonitor-exporter"]
CMD ["-config", "/etc/sysmonitor-exporter/config.yaml"]
