FROM debian:jessie
ENV VER=v0.5.1

MAINTAINER Clint Armstrong <clint@TrilliumITrmstrong.net>

ADD https://github.com/TrilliumIT/docker-drouter/releases/download/${VER}/docker-drouter /docker-drouter
RUN chmod +x /docker-drouter

ENTRYPOINT ["/docker-drouter"]
