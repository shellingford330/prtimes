#!/bin/sh

rm -rf ./public/image
mkdir ./public/image
chmod 755 ./public/image

docker compose down
docker image rm prtimes_app
docker compose up
