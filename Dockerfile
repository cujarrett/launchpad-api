# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o launchpad-api .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /app/launchpad-api /launchpad-api
EXPOSE 8080
ENTRYPOINT ["/launchpad-api"]
