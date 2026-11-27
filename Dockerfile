# syntax=docker/dockerfile:1@sha256:572cebbc2b15e6f84c077742952674889cb63cd5e9b80abdffcbe587b6f19183
FROM golang:1.27-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS build
WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o launchpad-api .

FROM ghcr.io/cujarrett/base-distroless:static-debian13@sha256:76a6ba0713608e3fcca0246b7c1989bbedfcff9809cab66fb6d9388bc091938c
COPY --from=build /app/launchpad-api /launchpad-api
EXPOSE 8080
ENTRYPOINT ["/launchpad-api"]
