FROM debian:jessie
ENV VER=v0.5.1

MAINTAINER Clint Armstrong <clint@clintarmstrong.net>

RUN apt-get -qq update && \
    apt-get -yqq install arptables kmod && \
    apt-get -qq clean && \
    rm -rf /var/lib/apt/lists/* /tmp/* /var/tmp/*

ADD https://github.com/clinta/docker-macvlan-router/releases/download/${VER}/docker-macvlan-router /docker-macvlan-router
RUN chmod +x /docker-macvlan-router

ENTRYPOINT ["/docker-macvlan-router"]
