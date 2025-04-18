# Copyright 2023 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     https://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# This Dockerfile is used to build various container images. There are a few
# targets:
#
# go-protoc: image with Golang, protoc and protoc-go pluggin
# base: image with Go modules download and protobuf built
# server: image for mixer server


FROM golang:1.20.7-alpine3.17 AS go-protoc

WORKDIR /mixer

# Install protoc
RUN apk update
RUN apk add bash protobuf-dev

# Install protobuf go plugin
ENV GO111MODULE=on
RUN go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.30.0
RUN go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.3.0


# Use pre-built go-protoc image from last step.
FROM gcr.io/datcom-ci/go-protoc:latest AS server
WORKDIR /mixer
# Docker cache: Download modules
COPY go.mod go.sum /mixer/
RUN go mod download -x

# Copy files
COPY proto/ proto
COPY internal/ internal

# Build protobuf
RUN protoc \
    --include_source_info \
    --include_imports \
    --proto_path=proto \
    --go_out=paths=source_relative:internal/proto \
    --go-grpc_out=paths=source_relative:internal/proto \
    --go-grpc_opt=require_unimplemented_servers=false \
    --experimental_allow_proto3_optional \
    --descriptor_set_out mixer-grpc.pb \
    proto/*.proto proto/**/*.proto

# Adding the grpc_health_probe
RUN GRPC_HEALTH_PROBE_VERSION=v0.4.7 && \
    wget -qO/bin/grpc_health_probe https://github.com/grpc-ecosystem/grpc-health-probe/releases/download/${GRPC_HEALTH_PROBE_VERSION}/grpc_health_probe-linux-amd64 && \
    chmod +x /bin/grpc_health_probe
# Build binary
COPY cmd/ cmd
COPY esp/ esp
RUN go build -o /go/bin/mixer cmd/main.go
RUN go build -o /go/bin/tools/clearcache cmd/tools/clearcache.go
ENTRYPOINT ["/go/bin/mixer"]