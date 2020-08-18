FROM node:14.4.0 as build

ENV PROJECT_ROOTDIR /app/

WORKDIR $PROJECT_ROOTDIR

COPY package*.json  $PROJECT_ROOTDIR

RUN npm i 

COPY . $PROJECT_ROOTDIR

RUN npm run build


FROM nginx

COPY --from=build /app/public /usr/share/nginx/html
