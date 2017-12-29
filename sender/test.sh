#!/bin/sh
set -ex

go build
openssl sha256 blob.bin
./sender 127.0.0.1:1234 blob.bin
