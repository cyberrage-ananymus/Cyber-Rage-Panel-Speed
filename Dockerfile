FROM golang:1.22-alpine AS gobuild
WORKDIR /build
COPY gorelay/ ./
RUN CGO_ENABLED=0 go build -o /gorelay .

FROM python:3.11-slim
WORKDIR /app

COPY requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt

COPY --from=gobuild /gorelay /usr/local/bin/gorelay
COPY *.py ./

ENV DATA_DIR=/data
RUN mkdir -p /data

EXPOSE 8000

CMD ["sh", "-c", "gorelay & python main.py"]
