FROM golang:1.22-bookworm AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/controlplane ./cmd/controlplane

FROM debian:bookworm-slim

RUN useradd --system --home-dir /app --create-home --shell /usr/sbin/nologin dockermonitor

WORKDIR /app
COPY --from=build /out/controlplane /usr/local/bin/controlplane

RUN mkdir -p /app/data/ca && chown -R dockermonitor:dockermonitor /app

USER dockermonitor

EXPOSE 8080 8443
VOLUME ["/app/data"]

ENTRYPOINT ["/usr/local/bin/controlplane"]