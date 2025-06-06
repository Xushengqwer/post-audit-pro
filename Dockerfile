# =================================================================
# Stage 1: The Builder Stage
# We use a specific Go version on Alpine Linux for a smaller build environment.
# =================================================================
FROM golang:1.23-alpine AS builder

# Set the working directory inside the container
WORKDIR /app

# Install necessary build tools
RUN apk add --no-cache git

# Copy the Go module files first. This leverages Docker's layer caching.
# Dependencies are only re-downloaded if go.mod or go.sum change.
COPY go.mod go.sum ./
RUN go mod download

# Copy the entire project source code into the container
COPY . .

# Build the Go application as a static binary.
# CGO_ENABLED=0 is important for creating a static binary without C dependencies.
# -ldflags="-w -s" strips debug information, which significantly reduces the binary size.
RUN CGO_ENABLED=0 GOOS=linux go build -a -ldflags="-w -s" -o /app/post_audit_service ./main.go


# =================================================================
# Stage 2: The Final Stage
# We start from a minimal Alpine image for a small and secure footprint.
# =================================================================
FROM alpine:latest

# Install ca-certificates, which are essential for your application to make
# secure TLS/HTTPS requests to external services like Aliyun or a TLS-enabled Kafka.
RUN apk --no-cache add ca-certificates

# Set the working directory
WORKDIR /app

# Copy only the compiled application binary from the builder stage.
COPY --from=builder /app/post_audit_service /app/post_audit_service

# Copy the configuration directory. Your application needs this at runtime
# to load its configuration, as specified by the --config flag.
COPY internal/config/ /app/internal/config/

# Define the entrypoint for the container. This is the command that will be executed.
ENTRYPOINT ["/app/post_audit_service"]

# Set the default command-line arguments. This points to your development
# configuration file. This can be easily overridden when you run the container,
# for example, to point to a production config file.
CMD ["--config=/app/internal/config/config.development.yaml"]