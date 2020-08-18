FROM golang:1.14-alpine AS build_backend

WORKDIR /app
COPY ./go/go.* ./
RUN go mod download
COPY ./go .
RUN go build -o /isucari api.go main.go

FROM node:14.4.0 as build_client

ENV PROJECT_ROOTDIR /app/
WORKDIR $PROJECT_ROOTDIR
COPY ./frontend/package*.json  $PROJECT_ROOTDIR
RUN npm i 
COPY ./frontend/ $PROJECT_ROOTDIR
RUN npm run build

FROM alpine:3.12.0

WORKDIR /app
RUN apk add --update ca-certificates imagemagick && \
    update-ca-certificates && \
    rm -rf /var/cache/apk/*
ENV DOCKERIZE_VERSION v0.6.1
RUN wget https://github.com/jwilder/dockerize/releases/download/$DOCKERIZE_VERSION/dockerize-alpine-linux-amd64-$DOCKERIZE_VERSION.tar.gz \
    && tar -C /usr/local/bin -xzvf dockerize-alpine-linux-amd64-$DOCKERIZE_VERSION.tar.gz \
    && rm dockerize-alpine-linux-amd64-$DOCKERIZE_VERSION.tar.gz

EXPOSE 4000

COPY --from=build_backend /isucari ./
COPY --from=build_client /app/public ../public

ENTRYPOINT ./isucari