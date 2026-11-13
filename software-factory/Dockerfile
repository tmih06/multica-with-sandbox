FROM golang:1.27.4-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/software-factory ./cmd/factory

FROM alpine:3.22
RUN apk add --no-cache ca-certificates kubectl
WORKDIR /app
COPY --from=builder /out/software-factory /app/software-factory

ENV FACTORY_ADDR=:8080
EXPOSE 8080
ENTRYPOINT ["/app/software-factory"]
