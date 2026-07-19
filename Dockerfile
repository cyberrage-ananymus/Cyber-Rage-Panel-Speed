FROM golang:1.22-alpine AS gobuild
WORKDIR /build
COPY gorelay/ ./
RUN go mod init cyber-rage-relay && go mod tidy && CGO_ENABLED=0 go build -o /gorelay .

FROM python:3.11-slim
WORKDIR /app

RUN apt-get update && apt-get install -y --no-install-recommends supervisor && rm -rf /var/lib/apt/lists/*

COPY requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt

COPY --from=gobuild /gorelay /usr/local/bin/gorelay
COPY *.py ./
COPY supervisord.conf /etc/supervisor/conf.d/supervisord.conf

ENV DATA_DIR=/data
RUN mkdir -p /data

EXPOSE 8000

CMD ["supervisord", "-c", "/etc/supervisor/conf.d/supervisord.conf"]
