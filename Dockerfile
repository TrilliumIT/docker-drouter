FROM debian:jessie
ENV VER=v0.5.1

MAINTAINER Clint Armstrong <clint@clintarmstrong.net>

ADD https://github.com/clinta/docker-drouter/releases/download/${VER}/docker-drouter /docker-drouter
RUN chmod +x /docker-drouter

ENTRYPOINT ["/docker-drouter"]
