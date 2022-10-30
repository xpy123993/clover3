#!/bin/sh

SRC_FOLDER=.
OUTPUT_FOLDER=releases
VERSION_NUMBER=${1:-debug}

[ ! -d $OUTPUT_FOLDER ] && mkdir $OUTPUT_FOLDER

for ARCH in arm arm64 amd64; do
CGO_ENABLED=0 GOOS=linux GOARCH=$ARCH go build -ldflags "-s -w" -o $OUTPUT_FOLDER/clover3-${VERSION_NUMBER}.${ARCH} $SRC_FOLDER
upx $OUTPUT_FOLDER/clover3-${VERSION_NUMBER}.${ARCH}
done

GOOS=windows GOARCH=amd64 go build -ldflags "-s -w" -o $OUTPUT_FOLDER/clover3-${VERSION_NUMBER}.exe $SRC_FOLDER