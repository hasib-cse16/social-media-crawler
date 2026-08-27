# ---- build ----
FROM golang:1.25-alpine AS build
WORKDIR /src

COPY go.mod ./
RUN go mod download

COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/socialstats ./cmd/api

# ---- run ----
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/socialstats /socialstats
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/socialstats"]
