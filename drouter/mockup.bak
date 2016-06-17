package drouter

import (
	"github.com/docker/engine-api/types/events"
	"github.com/vishvananda/netlink"
)

var ()

func mainLoop(shutdown chan string) {
	aggressiveTick := make(chan string)
	dockerEvent := make(chan *events.Message)
	routeSub := make(chan *netlink.RouteUpdate)

	for {
		select {
		case _ <- aggressiveTick:
			err := scanNetworks()
			if err != nil {
				log.Error(err)
			}
			continue

		case e <- dockerEvent:
			err := processEvent(e)
			if err != nil {
				log.Error(err)
			}
			continue

		case r <- routeSub:
			err := processRoute(r)
			if err != nil {
				log.Error(err)
			}
			continue

		case _ <- shutdown:
			break
		}
	}

	// do shutdown stuff
}

func scanNetworks() {
	/*
		for network in docker networks
			for ipam in network
				connected = false
					for route in routes
						if route.gw != nil
							continue
						if route.network.Contains(ipam.network)
							connected = true
							break
				if not connected
					go connectNetwork(network)
					break
	*/
	return nil
}

func processEvent(e *events.Message) error {
	a := e.Actor
	if a.ID != "network" {
		return nil
	}

	/*
		if a.Attributes["containerID"] == self
			continue

		if !network.drouter
			continue

		if attributes.action == connect
			if no direct to endpoint
			go connectNetwork(network)

		if attributes.action == disconnect
			for container in list containers
				if container == self
					continue
				if container.networks contains network
					return nil

			go disconnectNetwork(network)
	*/
	return nil
}

func processRoute(r *netlink.RouteUpdate) error {
	/*
		if r.Type == netlink.RTM_NEWROUTE
			for container in listcontainers
				if direct link to container
					addroutetocontainer(container, route)

		if r.Type == netlink.RTM_DELROTUE
			for container in listcontainers
				if direct link to container
					delroutefromcontainer(container, route)
	*/
}
