FROM golang:1.25-bookworm AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/flowmusic2api ./cmd/server

FROM debian:bookworm-slim

WORKDIR /app
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates tzdata wget \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/flowmusic2api /app/flowmusic2api
COPY web /app/web
COPY .env.example /app/.env.example
RUN mkdir -p /app/data /app/tmp

EXPOSE 8000
ENV FLOWMUSIC_ROOT=/app
HEALTHCHECK --interval=15s --timeout=3s --start-period=20s --retries=5 \
  CMD wget -qO- http://127.0.0.1:8000/health >/dev/null || exit 1
CMD ["/app/flowmusic2api"]
