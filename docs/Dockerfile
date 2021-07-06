FROM nginx:latest
RUN apt-get update; apt-get install -y nodejs npm; npm install -g markdown-styles;
COPY . /docs
WORKDIR /docs
RUN generate-md --layout github --input ./ --output /usr/share/nginx/html/
