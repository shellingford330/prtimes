#!/bin/sh

rm -rf ./public/image
mkdir ./public/image
chmod 755 ./public/image

docker compose down
docker volume rm prtimes_mysql
docker image rm prtimes_app
docker compose up
