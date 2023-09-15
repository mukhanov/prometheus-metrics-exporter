FROM golang:1.21-alpine

WORKDIR /app
EXPOSE 9999
ENV METRICS_READ_TIMEOUT ""

COPY go.mod ./
COPY go.sum ./

RUN go mod download

COPY src/* ./

RUN go build -o app

CMD [ "/app/app" ]