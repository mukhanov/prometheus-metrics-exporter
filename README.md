# prometheus-metrics-exporter

Aggregates metrics of all running containers on the host and exports them to prometheus through only one endpoint. So
you don't need to add each container address to scrape config. Metric names are transformed by appending a tag to
each of them. Container must have label "prometheus.enable=true". 

Example:

```yml
version: "3.3"
  services:
    traefik:
      image: traefik:v2.7
      ports:
        - "80:80"
      volumes:
        - /var/run/docker.sock:/var/run/docker.sock:ro
        - ./traefik.yml:/traefik.yml:ro
      labels:
        - "prometheus.enable=true"
        - "prometheus.port=9090"
        - "prometheus.context=/metrics"
        - "prometheus.protocol=http"

    nodeexporter:
      container_name: "nodeexporter"
      image: quay.io/prometheus/node-exporter:latest
      restart: unless-stopped
      labels:
        - "prometheus.enable=true"
        - "prometheus.port=9100"
        - "prometheus.context=/metrics"
        - "prometheus.protocol=http"

    prometheus-metrics-exporter:
      image: "ghcr.io/mukhanov/prometheus-metrics-exporter:master"
      container_name: "prometheus-metrics-exporter"
      restart: unless-stopped
      environment:
        METRICS_READ_TIMEOUT: 5000 #metrics read timeout
      ports:
        - 9999:9999
      volumes:
        - /var/run/docker.sock:/var/run/docker.sock
```