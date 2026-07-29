# ---- build ----
FROM golang:1.24-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o /out/tradebot ./cmd/tradebot

# ---- runtime ----
FROM alpine:3.20

RUN apk add --no-cache ca-certificates \
    && addgroup -S tradebot \
    && adduser -S tradebot -G tradebot

WORKDIR /app
COPY --from=build /out/tradebot /app/tradebot

# Bar cache lives here; mount a volume at this path to persist it across
# container restarts (see docker-compose.yml).
RUN mkdir -p /app/.cache && chown -R tradebot:tradebot /app
USER tradebot

ENTRYPOINT ["/app/tradebot"]
CMD ["run"]
