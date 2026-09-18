# syntax=docker/dockerfile:1

FROM node:24-alpine AS frontend
WORKDIR /src
COPY package.json package-lock.json ./
RUN npm ci
COPY web/src ./web/src
COPY web/templates ./web/templates
RUN mkdir -p web/static/css web/static/js && npm run build

FROM golang:1.26-alpine AS backend
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=frontend /src/web/static ./web/static
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/trs ./cmd/app

FROM alpine:3.23
RUN apk add --no-cache ca-certificates \
    && addgroup -S app \
    && adduser -S -G app app \
    && mkdir -p /app/data/slik \
    && chown -R app:app /app
COPY --from=backend /out/trs /usr/local/bin/trs
ENV APP_ENV=production
ENV SLIK_STORAGE_DIR=/app/data/slik
WORKDIR /app
USER app
EXPOSE 8080
ENTRYPOINT ["trs"]
