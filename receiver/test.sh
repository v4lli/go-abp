#!/bin/sh
set -ex

go build
./receiver
sha256sum blob.bin
