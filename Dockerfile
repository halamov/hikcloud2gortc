# hikcloud2gortc — стриминг Hik-Connect NVR на Go
#
# Сборка:  docker build -t hikcloud2gortc:TAG .
# Запуск:  docker run -d --name hikcloud2gortc \
#            -e HIK_USER=... -e HIK_PASSWORD=... \
#            -p 8000:8000 -v hikcloud2gortc-data:/data \
#            hikcloud2gortc:TAG

# ---- сборка ----
FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/hikcloud2gortc .

# ---- runtime ----
FROM debian:bookworm-slim
# ffmpeg нужен для remux Annex-B → TS и извлечения I-кадра;
# curl — для compose healthcheck (debian-slim без shell-утилит)
RUN apt-get update \
 && apt-get install -y --no-install-recommends ffmpeg ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=build /out/hikcloud2gortc /app/hikcloud2gortc
VOLUME ["/data"]
ENV DATA_DIR=/data
EXPOSE 8000
ENTRYPOINT ["/app/hikcloud2gortc"]
