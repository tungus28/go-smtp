# --- Этап 1: Сборка бинарного файла ---
FROM golang:1.22-alpine AS builder

# Устанавливаем рабочую директорию внутри контейнера
WORKDIR /app

# Сначала копируем файлы зависимостей для эффективного кэширования слоев Docker
# Копируем go.mod
COPY go.mod ./

# Создаем пустой go.sum, если его нет на хосте, чтобы go mod download не ругался
RUN touch go.sum

# Скачиваем зависимости
RUN go mod download

# Копируем исходный код
COPY . .

RUN go mod tidy

# Собираем оптимизированный бинарник без лишней отладочной информации
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o smtp-proxy main.go

# --- Этап 2: Финальный минимальный образ ---
FROM alpine:3.19

# Устанавливаем CA-сертификаты, так как наш прокси будет подключаться к Яндексу через TLS/SSL
RUN apk --no-cache add ca-certificates

WORKDIR /root/

# Копируем скомпилированный файл из первого этапа
COPY --from=builder /app/smtp-proxy .

# Открываем порт 7070, на котором работает наш сервер
EXPOSE 7070

# Запускаем приложение
CMD ["./smtp-proxy"]
