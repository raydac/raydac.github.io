#!/bin/bash
GOOS=js GOARCH=wasm go build -ldflags="-s -w" -o iam_whiteboard_wasm.wasm iam_whiteboard_wasm.go
