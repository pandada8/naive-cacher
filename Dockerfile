FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/naive-cacher .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/naive-cacher /naive-cacher
COPY config.example.yaml /etc/naive-cacher/config.yaml
EXPOSE 8080
ENTRYPOINT ["/naive-cacher", "-config", "/etc/naive-cacher/config.yaml"]
