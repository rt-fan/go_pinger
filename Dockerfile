FROM golang:1.23-alpine AS builder

# Install fping and git (git is needed for go mod download)
RUN apk add --no-cache fping git

WORKDIR /app

# Copy go.mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy the Go source file
COPY pinger.go .

# Build the application
RUN go build -o pinger pinger.go

# -----------------------------
# Final runtime image
# -----------------------------
FROM alpine:latest

# Install fping and SSL certificates (PostgreSQL needs them)
RUN apk --no-cache add fping ca-certificates tzdata

# Set timezone (Europe/Moscow)
RUN cp /usr/share/zoneinfo/Europe/Moscow /etc/localtime && echo "Europe/Moscow" > /etc/timezone

# Create non-root user
RUN adduser -D -s /bin/sh appuser

WORKDIR /app

# Copy binary
COPY --from=builder /app/pinger .

# Copy config file into container
COPY dbconfig.json /app/dbconfig.json

# (Optional) Copy empty last_ping.json if needed
# If file must persist → лучше через volume (см. ниже)
COPY last_ping.json /app/last_ping.json

# Permissions
RUN chown -R appuser:appuser /app

# USER appuser

CMD ["./pinger"]
