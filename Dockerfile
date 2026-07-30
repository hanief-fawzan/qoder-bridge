FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /qoder-bridge .

FROM alpine:3.22
COPY --from=build /qoder-bridge /usr/local/bin/qoder-bridge
EXPOSE 7100
ENTRYPOINT ["qoder-bridge", "run"]
