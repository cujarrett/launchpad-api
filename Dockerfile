# syntax=docker/dockerfile:1@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e
FROM golang:1.27-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS build
WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o launchpad-api .

FROM ghcr.io/cujarrett/base-distroless:static-debian13@sha256:57d71c4d9e6e68f81b301e7e7b0c500779bce9e35c665334e8c5134822fffc0d
COPY --from=build /app/launchpad-api /launchpad-api
EXPOSE 8080
ENTRYPOINT ["/launchpad-api"]
