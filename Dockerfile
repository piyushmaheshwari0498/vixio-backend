# Start with a lightweight Go image
FROM golang:1.23-alpine

# Install FFmpeg (Required for video processing)
RUN apk update && apk add --no-cache ffmpeg

# Set working directory
WORKDIR /app

# Copy dependency files first (better caching)
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the app code
COPY . .

# Build the Go app
RUN go build -o main .

# Create the output directory for videos
RUN mkdir -p output

# Expose the port
EXPOSE 8080

# Run the app
CMD ["./main"]