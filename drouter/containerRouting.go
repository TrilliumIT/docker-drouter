package drouter

import (
	"net"
	log "github.com/Sirupsen/logrus"
	dockertypes "github.com/docker/engine-api/types"
	dockerevents "github.com/docker/engine-api/types/events"
	"golang.org/x/net/context"
	"github.com/vishvananda/netlink"
	"github.com/vdemeester/docker-events"
)

//ensure all container's routes are synced
func (dr *DistributedRouter) syncAllRoutes() error {
	log.Debug("syncAllRoutes()")

	//Loop through all containers and sync the routes
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

		//don't try to set routes for ourself
		if cjson.State.Pid == dr.pid {
			continue
		}

		//skip containers running with --net=host
		if cjson.HostConfig.NetworkMode == "host" {
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
			err = dr.syncContainerRoutes(ch)
		}
		if err != nil {
			log.Error(err)
			continue
		}
	}
	
	return nil
}

//adds all known routes, and remove unknown routes for provided container
func (dr *DistributedRouter) syncContainerRoutes(ch *netlink.Handle) error {
	log.Debug("syncContainerRoutes()")

	//Loop through all container routes, ensure each one is known otherwise scrub it.
	log.Debug("Retrieving container routes.")
	routes, err := ch.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		log.Error("Failed to get container routes for scrubbing.")
		return err
	}

	log.Debug("Scrubbing uknown routes.")
	for _, r := range routes {
		//don't mess with the default route
		if r.Dst == nil {
			continue
		}

		//don't mess with directly attached networks
		if r.Gw == nil {
			continue
		}

		known := false
		Networks:
		for _, drn := range dr.networks {
			for _, sn := range drn.subnets {
				if sn.Contains(r.Gw) {
					//route is for a known subnet
					known = true
					break Networks
				}
			}
		}

		if dr.p2p.network.Contains(r.Gw) {
			//route is for the p2p subnet
			known = true
		}

		if !known {
			log.Infof("Attempting to delete unknown route for: %v", r.Dst)
			err := ch.RouteDel(&r)
			if err != nil {
				log.Error(err)
				continue
			}
		}
	}

	if len(dr.networks) == 0 {
		//bail since we can't route without connections
		//happens during Close()
		return nil
	}

	//get the drouter gateway IP for this container
	gateway, err := dr.getContainerPathIP(ch)
	if err != nil {
		return err
	}

	//get link index for the gateway network
	log.Debugf("Getting container route to %v", gateway)
	lindex, err := ch.RouteGet(gateway)
	if err != nil {
		return err
	}

	//Loop through all static routes, ensure each one is installed in the container
	log.Info("Syncing static routes.")
	//add routes for all the static routes
	StaticRoute:
	for _, sr := range dr.staticRoutes {
		croute, err := ch.RouteGet(sr.IP)
		if err == nil {
			for _, r := range croute {
				//don't skip locally attached route because false positives could occur
				//just let netlink issue a warning that the static route failed to add
				if r.Gw.Equal(gateway) {
					//skip existing route
					continue StaticRoute
				}
			}
		}

		route := &netlink.Route{
			LinkIndex: lindex[0].LinkIndex,
			Dst: sr,
			Gw: gateway,
		}

		log.Infof("Adding static route to %v via %v.", sr, gateway)
		err = ch.RouteAdd(route)
		if err != nil {
			log.Warning(err)
			continue
		}
	}

	//Loop through all discovered networks, ensure each one is installed in the container
	log.Info("Syncing discovered routes.")
	for _, drn := range dr.networks {
		//add routes for all the subnets of this discovered network
		Subnet:
		for _, sn := range drn.subnets {
			croute, err := ch.RouteGet(sn.IP)
			if err == nil {
				for _, r := range croute {
					if r.Gw == nil || r.Gw.Equal(gateway) {
						//skip existing or local route
						continue Subnet
					}
				}
			}

			route := &netlink.Route{
				LinkIndex: lindex[0].LinkIndex,
				Dst: sn,
				Gw: gateway,
			}

			log.Infof("Adding discovered route to %v via %v.", sn, gateway)
			err = ch.RouteAdd(route)
			if err != nil {
				log.Error(err)
				continue
			}
		}
	}

	return nil
}

//sets the provided container's default route to the provided gateway, or ourselves if gw==nil
func (dr *DistributedRouter) replaceContainerGateway(ch *netlink.Handle, gw net.IP) error {
	log.Debugf("replaceContainerGateway(%v, %v)", ch, gw)

	var defr *netlink.Route
	//replace containers default gateway with drouter
	routes, err := ch.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return err
	}
	log.Debugf("container routes: %v", routes)
	for _, r := range routes {
		if r.Dst != nil {
			// Not the container gateway
			continue
		}

		defr = &r
	}

	//if gateway was not specified, discover ourselves
	if gw == nil || gw.Equal(net.IP{}) {
		gw, err = dr.getContainerPathIP(ch)
		if err != nil {
			log.Error("Failed to determine the drouter IP to set as the default route.")
			return err
		}
	}

	//bail if the container gateway is already set to gw
	if gw.Equal(defr.Gw) {
		log.Debug("Container default route is already set to: %v", gw)
		return nil
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

// Watch for container events to add ourself to the container routing table.
func (dr *DistributedRouter) watchEvents() error {
	log.Debug("Watching for container events.")
	errChan := events.Monitor(context.Background(), dr.dc, dockertypes.EventsOptions{}, func(event dockerevents.Message) {
		if event.Type != "network" { return }
		if event.Action != "connect" { return }
		// don't run on self events
		if event.Actor.Attributes["container"] == dr.selfContainer.ID { return }

		//TODO: join the network, if we are not connected
		
		// don't run if this network is not a "drouter" network
		if _, ok := dr.networks[event.Actor.ID]; !ok {
			return
		}

		log.Debugf("Event.Actor: %v", event.Actor)

		//get new containers info
		containerInfo, err := dr.dc.ContainerInspect(context.Background(), event.Actor.Attributes["container"])
		if err != nil {
			log.Error(err)
			return
		}
		log.Debugf("containerInfo: %v", containerInfo)

		//get new container's namespace handle
		containerHandle, err := netlinkHandleFromPid(containerInfo.State.Pid)
		if err != nil {
			log.Error(err)
			return
		}

		if dr.localGateway {
			dr.replaceContainerGateway(containerHandle, nil)
		} else {
			dr.syncContainerRoutes(containerHandle)
		}
	})

	if err := <-errChan; err != nil {
		return err
	}

	return nil
}

//returns a drouter IP that is on some same network as provided container
func (dr *DistributedRouter) getContainerPathIP(ch *netlink.Handle) (net.IP, error) {
	var gateway net.IP
	addrs, err := ch.AddrList(nil, netlink.FAMILY_V4)
	if err != nil {
		log.Error("Failed to list container addresses.")
		return nil, err
	}

	for _, addr := range addrs {
		if addr.Label == "lo" {
			continue
		}

		log.Debugf("Getting my route to container IP: %v", addr.IP)
		src, err := dr.selfNamespace.RouteGet(addr.IP)
		if err != nil {
			log.Error(err)
			continue
		}
		if src[0].Gw == nil {
			gateway = src[0].Src
		}
	}
	return gateway, nil
}
