# Docker Distributed Router

This container provides a distributed router for docker containers.

## Quick Start

```bash
docker run --pid=host --privileged -it -v /var/run/docker.sock:/var/run/docker.sock trilliumit/docker-drouter
docker network create -o drouter=true drouter-net
docker run -it --net=drouter-net busybox
```

At this point you will be in a busybox container on the `drouter-net` with a gateway address that lives on the drouter. The drouter will route traffic between this container and any other containers on networks with `drouter=true` and with any other network your docker host can access.

`--pid=host`, `--privileged` and `-v /var/run/docker.sock:/var/run/docker.sock` are required so that the router can montior docker events and enter container namespaces as they spin up to change their default gateways.

### How it works

When drouter spins up it creates a veth p2p link between the host and itslef.

Drouter then watches for events on the docker socket. When a container spins up on a network which has the `drouter=true` option, drouter joins the same network, then enters the containers namespace and sets itself as the default gateway for the container. It also enters the host namespace and injects a route onto the host ot the container network via it's p2p link.


### Why would I want this

In a clustered environment you can run drouter on every node in your cluster and have 1 hop routing between all of your containers.


### Options

#### -d --debug

Debug mode

#### --ip-offset

An offset for the ip address for the drouter container. Set to 1 it will choose the first IP address in the network. Set to -1 to choose the last IP. If not set, the docker IPAM will choose an address.

#### --aggressive

Join all networks with drouter=true whether or not there are any existing containers on them.


#### --disable-p2p

Disable the p2p link with the host.

#### --p2p-addr

Use a specific network for p2p communication. Default is 172.29.255.252/30.


### Future plans

The plan is for allow routing between all container networks without having to join all of them in a cluster. This will be accomplished by using the docker key value store to dynamically generate routes to networks on other hosts.
