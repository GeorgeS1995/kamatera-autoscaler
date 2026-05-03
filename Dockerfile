FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/autoscaler ./cmd/autoscaler

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/autoscaler /autoscaler
USER nonroot:nonroot
ENTRYPOINT ["/autoscaler"]
