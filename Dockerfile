FROM debian:jessie
ENV VER=v0.5.1

MAINTAINER Clint Armstrong <clint@clintarmstrong.net>

ADD https://github.com/clinta/docker-macvlan-router/releases/download/${VER}/docker-macvlan-router /docker-macvlan-router
RUN chmod +x /docker-macvlan-router

ENTRYPOINT ["/docker-macvlan-router"]
