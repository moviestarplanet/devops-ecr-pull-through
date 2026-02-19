FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build

WORKDIR /app

COPY go.mod go.sum ./
COPY cmd/ cmd/

ARG TARGETOS TARGETARCH

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o bin/mutation-webhook cmd/*.go

FROM gcr.io/distroless/base-debian13:nonroot

COPY --from=build /app/bin/mutation-webhook /

ENTRYPOINT [ "/mutation-webhook" ]
