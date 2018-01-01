#!/bin/sh
set -ex

go build
./receiver unrealiable
sha256sum blob.bin
