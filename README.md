# Docker Distributed Router

This container provides a distributed router for docker containers.

## Use Case

docker-drouter is a container that is designed to run on a set of hosts with the [docker-vxlan-plugin](https://github.com/TrilliumIT/docker-vxlan-plugin) in `global` mode. The container will dynamically discover and connect to all existing vxlans in your cluster, adjust the routing tables for your containers, and enable routing between vxlans, always taking the shortest path to get to the destination. Currently, with docker-drouter, an external gateway with access to each vxlan is still required for routing outside of your container cluster.

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

Drouter then watches for events on the docker socket. When a container spins up on a network which has the `drouter=true` option, drouter joins the same network, then enters the containers namespace and sets itself as the default gateway for the container. It also enters the host namespace and injects a route onto the host of the container network via it's p2p link.

### Why would you want this

In a clustered environment you can run drouter on every node in your cluster and have shortest path routing between all of your containers.

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

### Roadmap

* Implement passive mode: Dynamically join vxlans only when sibling containers are actually running on those vxlans, rather than joining everything.
* Implement default gateway mode: Allowing drouter to exit the container cluster through the host's default route. Optionally with masquerading. (A drop in replacement for Docker Overlay, without mutli-homed containers)
