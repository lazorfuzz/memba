FROM golang:1.24-alpine AS build
RUN apk add --no-cache git
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/memd ./cmd/memd \
 && CGO_ENABLED=0 go build -o /out/memctl ./cmd/memctl \
 && CGO_ENABLED=0 go build -o /out/mem-bench ./cmd/mem-bench

FROM alpine:3.20
RUN apk add --no-cache git ca-certificates
WORKDIR /app
COPY --from=build /out/memd /out/memctl /out/mem-bench /usr/local/bin/
COPY configs /app/configs
EXPOSE 8080
ENTRYPOINT []
CMD ["/usr/local/bin/memd", "--role", "all"]
