# syntax=docker/dockerfile:1@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e
FROM golang:1.27-alpine@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS build
WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o launchpad-api .

FROM ghcr.io/cujarrett/base-distroless:static-debian13@sha256:a359c6ce58f19d70594119fa4d2233f0d27d93bce787328a4936fd1f995cc779
COPY --from=build /app/launchpad-api /launchpad-api
EXPOSE 8080
ENTRYPOINT ["/launchpad-api"]
