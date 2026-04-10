#!/bin/bash
set -e

APP_NAME="agent-proxy"
BUILD_DIR="build"

mkdir -p "$BUILD_DIR"

echo "Building $APP_NAME..."

# Build for current platform
go build -ldflags="-s -w" -o "$BUILD_DIR/$APP_NAME" .

echo "Built: $BUILD_DIR/$APP_NAME"

# Cross-compile if requested
if [ "$1" = "all" ]; then
    echo "Cross-compiling..."
    GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o "$BUILD_DIR/${APP_NAME}-linux-amd64" .
    GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o "$BUILD_DIR/${APP_NAME}-linux-arm64" .
    GOOS=darwin GOARCH=amd64 go build -ldflags="-s -w" -o "$BUILD_DIR/${APP_NAME}-darwin-amd64" .
    GOOS=darwin GOARCH=arm64 go build -ldflags="-s -w" -o "$BUILD_DIR/${APP_NAME}-darwin-arm64" .
    echo "All builds complete."
fi
