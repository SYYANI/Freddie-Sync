# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.26.10 AS build

WORKDIR /src
ENV GOTOOLCHAIN=local
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY admin/ ./admin/

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=container
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.buildVersion=${VERSION}" -o /out/freddie-sync .

FROM scratch
COPY --from=build /out/freddie-sync /usr/local/bin/freddie-sync
USER 10001:10001
ENV GOMEMLIMIT=128MiB GOGC=50
ENTRYPOINT ["/usr/local/bin/freddie-sync"]
CMD ["version"]
