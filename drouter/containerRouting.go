package drouter

import (
	"net"
	"fmt"
	"errors"
	log "github.com/Sirupsen/logrus"
	dockertypes "github.com/docker/engine-api/types"
	dockerevents "github.com/docker/engine-api/types/events"
	"golang.org/x/net/context"
	"github.com/vishvananda/netlink"
	"github.com/vdemeester/docker-events"
)

func (dr *DistributedRouter) addNetworkRoutes(drn *drNetwork) error {
	//Loop through all containers and set the routes
	containers, err := dr.dc.ContainerList(context.Background(), dockertypes.ContainerListOptions{})
	if err != nil {
		return err
	}

	for _, c := range containers {
		cjson, err := dr.dc.ContainerInspect(context.Background(), c.ID)
		if err != nil {
			log.Error(err)
			continue
		}

		ch, err := netlinkHandleFromPid(cjson.State.Pid)
		if err != nil {
			log.Error(err)
			continue
		}

		err = dr.addContainerRoute(ch, drn)
		if err != nil {
			log.Error(err)
			continue
		}
	}
	
	return nil
}

func (dr *DistributedRouter) initContainers() error {
	//Loop through all containers and set the routes
	containers, err := dr.dc.ContainerList(context.Background(), dockertypes.ContainerListOptions{})
	if err != nil {
		return err
	}

	for _, c := range containers {
		cjson, err := dr.dc.ContainerInspect(context.Background(), c.ID)
		if err != nil {
			log.Error(err)
			continue
		}

		ch, err := netlinkHandleFromPid(cjson.State.Pid)
		if err != nil {
			log.Error(err)
			continue
		}

		if dr.localGateway {
			err = dr.replaceContainerGateway(ch, nil)
		} else {
			err = dr.addContainerRoutes(ch)
		}
		if err != nil {
			log.Error(err)
			continue
		}
	}
	
	return nil
}

func (dr *DistributedRouter) addContainerRoutes(ch *netlink.Handle) error {
	if len(dr.summaryNets) > 0 {
		//TODO: insert summary routes
	} else {
		//Loop through all networks, call addContainerRoute()
		for _, drn := range dr.networks {
			err := dr.addContainerRoute(ch, drn)
			if err != nil {
				log.Error(err)
				continue
			}
		}
	}
	return nil
}

func (dr *DistributedRouter) addContainerRoute(ch *netlink.Handle, drn *drNetwork) error {
	//Put the prefix into the container routing table pointing back to the drouter
	if !drn.drouter {
		return errors.New("Refusing to add container route for non drouter network.")
	}

	if !drn.connected {
		return errors.New("Refusing to add container route for non connected network.")
	}

	//get link index, and container IP
	cdr, err := ch.RouteGet(drn.ip)
	if err != nil {
		return err
	}

	//get drouter ip on container network
	src, err := dr.selfNamespace.RouteGet(cdr[0].Src)
	if err != nil {
		return err
	}

	route := &netlink.Route{
		LinkIndex: cdr[0].LinkIndex,
		Dst: drn.subnet,
		Gw: src[0].Src,
	}

	return ch.RouteAdd(route)
}

func (dr *DistributedRouter) replaceContainerGateway(ch *netlink.Handle, gw net.IP) error {
	//replace containers default gateway with drouter
	routes, err := ch.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return err
	}
	var defr *netlink.Route
	log.Debugf("container routes: %v", routes)
	for _, r := range routes {
		// The container gateway
		if r.Dst != nil {
			continue
		}

		defr = &r
	}

	//if we don't have a gateway specified, set discover ourselves
	if gw == nil || gw.Equal(net.IP{}) {
		// Default route has no src, need to get the route to the gateway to get the src
		src_route, err := ch.RouteGet(defr.Gw)
		if err != nil {
			return err
		}
		if len(src_route) == 0 {
			serr := fmt.Sprintf("No route found in container to the containers existing gateway: %v", defr.Gw)
			return errors.New(serr)
		}

		// Get the route from gw-container back to the container, 
		// this src address will be used as the container's gateway
		gw_rev_route, err := dr.selfNamespace.RouteGet(src_route[0].Src)
		if err != nil {
			return err
		}
		if len(gw_rev_route) == 0 {
			serr := fmt.Sprintf("No route found back to container ip: %v", src_route[0].Src)
			return errors.New(serr)
		}
		gw = gw_rev_route[0].Src
	}

	log.Debugf("Remove existing default route: %v", defr)
	err = ch.RouteDel(defr)
	if err != nil {
		return err
	}

	defr.Gw = gw
	err = ch.RouteAdd(defr)
	if err != nil {
		return err
	}
	log.Debugf("Default route changed to: %v", defr)

	return nil
}

func (dr *DistributedRouter) deinitContainers() error {
	//Loop through all containers, remove drouter routes
	containers, err := dr.dc.ContainerList(context.Background(), dockertypes.ContainerListOptions{})
	if err != nil {
		return err
	}

	for _, c := range containers {
		cjson, err := dr.dc.ContainerInspect(context.Background(), c.ID)
		if err != nil {
			log.Error(err)
			continue
		}

		ch, err := netlinkHandleFromPid(cjson.State.Pid)
		if err != nil {
			log.Error(err)
			continue
		}

		//replace container gateway with it's network's gateway
		if dr.localGateway {
			err = dr.replaceContainerGateway(ch, dr.networks[c.NetworkSettings.Networks[c.HostConfig.NetworkMode].NetworkID].gateway)
		} else {
			err = dr.delContainerRoutes(ch)
		}
		if err != nil {
			log.Error(err)
			continue
		}
	}
	return nil
}

func (dr *DistributedRouter) delContainerRoutes(ch *netlink.Handle) error {
	if len(dr.summaryNets) > 0 {
		//remove summary routes
	} else {
		//Loop through all networks, call delContainerRoute()
		for _, drn := range dr.networks {
			err := dr.delContainerRoute(ch, drn)
			if err != nil {
				log.Error(err)
				continue
			}
		}
	}
	return nil
}

func (dr *DistributedRouter) delNetworkRoutes(drn *drNetwork) error {
	//Loop through all containers and set the routes
	containers, err := dr.dc.ContainerList(context.Background(), dockertypes.ContainerListOptions{})
	if err != nil {
		return err
	}

	for _, c := range containers {
		cjson, err := dr.dc.ContainerInspect(context.Background(), c.ID)
		if err != nil {
			log.Error(err)
			continue
		}

		ch, err := netlinkHandleFromPid(cjson.State.Pid)
		if err != nil {
			log.Error(err)
			continue
		}

		err = dr.delContainerRoute(ch, drn)
		if err != nil {
			log.Error(err)
			continue
		}
	}
	
	return nil
}

func (dr *DistributedRouter) delContainerRoute(ch *netlink.Handle, drn *drNetwork) error {
	//get link index, and container IP
	cdr, err := ch.RouteGet(drn.ip)
	if err != nil {
		return err
	}

	//get drouter ip on container network
	src, err := dr.selfNamespace.RouteGet(cdr[0].Src)
	if err != nil {
		return err
	}

	route := &netlink.Route{
		LinkIndex: cdr[0].LinkIndex,
		Dst: drn.subnet,
		Gw: src[0].Src,
	}

	return ch.RouteDel(route)
}

// Watch for container events to add ourself to the container routing table.
func (dr *DistributedRouter) watchEvents() {
	errChan := events.Monitor(context.Background(), dr.dc, dockertypes.EventsOptions{}, func(event dockerevents.Message) {
		if event.Type != "network" { return }
		if event.Action != "connect" { return }
		// don't run on self events
		if event.Actor.Attributes["container"] == dr.selfContainer.ID { return }

		log.Debugf("Network connect event detected. Syncing Networks.")
		err := dr.syncNetworks()
		if err != nil {
			log.Error(err)
		}
		
		// don't run if this network is not a "drouter" network
		if !dr.networks[event.Actor.ID].drouter { return }
		log.Debugf("Event.Actor: %v", event.Actor)

		//get new containers info
		containerInfo, err := dr.dc.ContainerInspect(context.Background(), event.Actor.Attributes["container"])
		if err != nil {
			log.Error(err)
			return
		}
		log.Debugf("containerInfo: %v", containerInfo)

		//get new containers namespace handle
		containerHandle, err := netlinkHandleFromPid(containerInfo.State.Pid)
		if err != nil {
			log.Error(err)
			return
		}

		if dr.localGateway {
			dr.replaceContainerGateway(containerHandle, nil)
		} else {
			dr.addContainerRoutes(containerHandle)
		}

	})
	if err := <-errChan; err != nil {
		log.Error(err)
	}
}
