#!/bin/sh

docker compose down
docker image rm prtimes_app
docker compose up
