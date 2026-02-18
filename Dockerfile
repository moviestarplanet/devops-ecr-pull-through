FROM golang:1.25-alpine

WORKDIR /app

COPY . .

ARG TARGETOS=linux
ARG TARGETARCH=amd64

RUN CGO_ENABLED=0 GOARCH=${TARGETARCH} GOOS=${TARGETOS} go build -o bin/mutation-webhook cmd/*.go

FROM gcr.io/distroless/base-debian12

COPY --from=0 /app/bin/mutation-webhook /

ENTRYPOINT [ "/mutation-webhook" ]
