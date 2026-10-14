# Контекст сборки: каталог services/ (нужен для replace на ../cbl-kit).
# Сборка: docker build -f cbl-mock-gateway/Dockerfile -t cbl/mock-gateway:dev .
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY cbl-kit/ /src/cbl-kit/
COPY cbl-mock-gateway/ /src/cbl-mock-gateway/
WORKDIR /src/cbl-mock-gateway
RUN go mod download
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/mock-gateway ./cmd/mock-gateway

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/mock-gateway /app/mock-gateway
EXPOSE 8120 9320
ENTRYPOINT ["/app/mock-gateway"]
