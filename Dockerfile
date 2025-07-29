FROM --platform=$BUILDPLATFORM golang:1.24.4-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /app/castai-pdb-controller-bin ./cmd/main.go

FROM alpine:3.19
WORKDIR /app
COPY --from=builder /app/castai-pdb-controller-bin .
CMD ["./castai-pdb-controller-bin"]

