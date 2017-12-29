#!/bin/sh
set -eux

go build
./sender file
