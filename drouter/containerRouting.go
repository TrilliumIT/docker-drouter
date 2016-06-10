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

	if len(dr.summaryNets) > 0 {
		log.Error("Summary nets are not implemented yet.")
		//TODO: insert summary routes
		return nil
	}

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
			log.Debugf("Found default route: %v", r)
			continue
		}

		//don't mess with directly attached networks
		if r.Gw == nil {
			log.Debugf("Found direct route: %v", r)
			continue
		}

		known := false
		Networks:
		for _, drn := range dr.networks {
			for _, sn := range drn.subnets {
				if sn.Contains(r.Gw) {
					log.Debugf("Found known route: %v", r)
					known = true
					break Networks
				}
			}
		}

		if !known {
			log.Infof("Attempting delete unknown route: %v", r)
			err := ch.RouteDel(&r)
			if err != nil {
				log.Error(err)
				continue
			}
		}
	}

	//Loop through all known networks, ensure each one is installed in the container
	log.Info("Syncing routes.")
	for _, drn := range dr.networks {
		//get the drouter gateway IP for this container
		gateway, err := dr.getContainerPathIP(ch)
		if err != nil {
			log.Error(err)
			continue
		}

		//get link index for the gateway network
		log.Debugf("Getting container route to %v", gateway)
		lindex, err := ch.RouteGet(gateway)
		if err != nil {
			log.Error(err)
			continue
		}

		//add routes for all the subnets of this network
		Subnet:
		for _, sn := range drn.subnets {
			croute, err := ch.RouteGet(sn.IP)
			if err == nil {
				for _, r := range croute {
					if r.Gw == nil || r.Gw.Equal(gateway) {
						log.Debugf("Skipping existing route: %v", r)
						continue Subnet
					}
				}
			}

			route := &netlink.Route{
				LinkIndex: lindex[0].LinkIndex,
				Dst: sn,
				Gw: gateway,
			}

			log.Infof("Adding route to %v via %v .", sn, gateway)
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

		log.Debugf("Network connect event detected. Syncing Networks.")
		err := dr.syncNetworks()
		if err != nil {
			log.Error(err)
		}
		
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

/*
//return filtered list of container handles (removes ourself and --net=host containers)
func (dr *DistributedRouter) getContainerHandleList() ([]*netlink.Handle, error) {

	mcs := make([]*netlink.Handle, 1)
	//get filtered list of containers
	containers, err := dr.dc.ContainerList(context.Background(), dockertypes.ContainerListOptions{})
	if err != nil {
		return nil, err
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

		mcs = append(mcs, ch)
	}

	return mcs, nil
}
*/

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
