FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/mock-gateway ./cmd/mock-gateway

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/mock-gateway /app/mock-gateway
EXPOSE 8120 9320
ENTRYPOINT ["/app/mock-gateway"]