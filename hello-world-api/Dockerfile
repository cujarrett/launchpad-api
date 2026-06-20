FROM golang:1.26-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o hello-world-api .

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /app/hello-world-api /hello-world-api
EXPOSE 8080
ENTRYPOINT ["/hello-world-api"]
