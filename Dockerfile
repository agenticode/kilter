# Build a fully static kilter binary, ship it on distroless.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=none
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
    -o /kilter ./cmd/kilter

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /kilter /kilter
USER nonroot:nonroot
ENTRYPOINT ["/kilter"]
