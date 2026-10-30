FROM golang:1.26.0-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/backup-manager ./cmd/backup-manager

FROM alpine:3.23
RUN apk add --no-cache ca-certificates restic tzdata \
    && addgroup -S -g 10001 app && adduser -S -u 10001 -G app app
COPY --from=build /out/backup-manager /usr/local/bin/backup-manager
USER 10001:10001
EXPOSE 8080
ENTRYPOINT ["backup-manager"]
CMD ["serve"]
