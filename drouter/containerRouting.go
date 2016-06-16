package drouter

import (
	"fmt"
	log "github.com/Sirupsen/logrus"
	dockertypes "github.com/docker/engine-api/types"
	dockerevents "github.com/docker/engine-api/types/events"
	"github.com/vdemeester/docker-events"
	"github.com/vishvananda/netlink"
	"golang.org/x/net/context"
	"net"
	"strings"
)

//adds all known routes for provided container
func (dr *DistributedRouter) addAllContainerRoutes(ch *netlink.Handle) error {
	log.Debug("addAllContainerRoutes()")

	//get the drouter gateway IP for this container
	gateway, err := dr.getContainerPathIP(ch)
	if err != nil {
		log.Error("It seems as though we don't have a common network with the container. Can't route for it.")
		return err
	}

	//Loop through all static routes, ensure each one is installed in the container
	log.Info("Syncing static routes.")
	//add routes for all the static routes
	for _, sr := range dr.staticRoutes {
		err = dr.addContainerRoute(ch, sr, gateway)
		if err != nil {
			log.Warning(err)
			continue
		}
	}

	//Loop through all discovered networks, ensure each one is installed in the container
	//Unless it is covered by a static route already
	log.Info("Syncing discovered routes.")
	for _, drn := range dr.networks {
		if !drn.drouter {
			continue
		}
		if !drn.connected {
			continue
		}

		//add routes for all the subnets of this discovered network
	Subnets:
		for _, sn := range drn.subnets {
			for _, sr := range dr.staticRoutes {
				if sr.Contains(sn.IP) {
					srlen, srbits := sr.Mask.Size()
					snlen, snbits := sn.Mask.Size()
					if srlen <= snlen && srbits == snbits {
						log.Debugf("Skipping route %v covered by %v.", sn, sr)
						break Subnets
					}
				}
			}
			err = dr.addContainerRoute(ch, sn, gateway)
			if err != nil {
				log.Error(err)
				continue
			}
		}
	}

	return nil
}

func (dr *DistributedRouter) addContainerRoute(ch *netlink.Handle, prefix *net.IPNet, gateway net.IP) error {
	//get link index for the gateway network
	log.Debugf("Getting container route to %v", gateway)
	lindex, err := ch.RouteGet(gateway)
	if err != nil {
		return err
	}

	route := &netlink.Route{
		LinkIndex: lindex[0].LinkIndex,
		Dst:       prefix,
		Gw:        gateway,
	}

	log.Infof("Adding route to %v via %v.", prefix, gateway)
	err = ch.RouteAdd(route)
	if err != nil {
		if !strings.Contains(err.Error(), "file exists") {
			return err
		}
	}

	return nil
}

func (dr *DistributedRouter) delContainerRoutes(ch *netlink.Handle, prefix *net.IPNet) error {
	//get all container routes
	routes, err := ch.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		log.Error("Failed to get container route table.")
		return err
	}

	//get all drouter ips
	ips, err := dr.selfNamespace.AddrList(nil, netlink.FAMILY_V4)
	if err != nil {
		log.Error("Failed to get drouter ip addresses.")
		return err
	}

	for _, r := range routes {
		if r.Dst == nil {
			continue
		}
		if !prefix.Contains(r.Dst.IP) {
			continue
		}

		for _, ipaddr := range ips {
			if r.Gw.Equal(ipaddr.IP) {
				err := ch.RouteDel(&r)
				if err != nil {
					log.Errorf("Failed to delete container route to %v via %v", r.Dst, r.Gw)
					continue
				}
			}
		}
	}

	return nil
}

//sets the provided container's default route to the provided gateway
func (dr *DistributedRouter) replaceContainerGateway(ch *netlink.Handle, gateway net.IP) error {
	log.Debugf("replaceContainerGateway(%v, %v)", ch, gateway)

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

	//bail if the container gateway is already set to gateway
	if gateway.Equal(defr.Gw) {
		return nil
	}

	log.Debugf("Remove existing default route: %v", defr)
	err = ch.RouteDel(defr)
	if err != nil {
		return err
	}

	if gateway == nil || gateway.Equal(net.IP{}) {
		return nil
	}

	defr.Gw = gateway
	err = ch.RouteAdd(defr)
	if err != nil {
		return err
	}
	log.Debugf("Default route changed to: %v", defr)

	return nil
}

// called during a network connect event
func (dr *DistributedRouter) containerNetworkConnectEvent(containerID, networkID string) error {
	//first, see if we are connected
	if !dr.networks[networkID].connected {
		//connect now, which handles adding routes
		return dr.connectNetwork(networkID)
	}

	//let's push our routes into this new container
	//get new containers info
	containerInfo, err := dr.dc.ContainerInspect(context.Background(), containerID)
	if err != nil {
		return err
	}
	log.Debugf("containerInfo: %v", containerInfo)

	//get new container's namespace handle
	ch, err := netlinkHandleFromPid(containerInfo.State.Pid)
	if err != nil {
		return err
	}

	gateway, err := dr.getContainerPathIP(ch)
	if err != nil {
		log.Error("Failed to get container path IP.")
		return err
	}

	if dr.localGateway {
		return dr.replaceContainerGateway(ch, gateway)
	} else {
		return dr.addAllContainerRoutes(ch)
	}
}

// called during a network disconnect event
func (dr *DistributedRouter) containerNetworkDisconnectEvent(containerID, networkID string) error {
	//TODO: remove all routes from container, just in case it's an admin disconnect, rather than a stop
	//TODO: then, test for other possible connections to the container,
	//TODO: and if so, re-install the routes through that gateway

	//if not aggressive mode, then we disconnect from the network if this is the last connected container
	if !dr.aggressive {
		inUse := false
		//loop through all the containers
		containers, err := dr.dc.ContainerList(context.Background(), dockertypes.ContainerListOptions{})
		if err != nil {
			return err
		}

	Containers:
		for _, c := range containers {
			if c.HostConfig.NetworkMode == "host" {
				continue
			}
			if c.ID == dr.selfContainerID {
				continue
			}

			for _, n := range c.NetworkSettings.Networks {
				if networkID == n.NetworkID {
					inUse = true
					break Containers
				}
			}
		}

		if !inUse {
			return dr.disconnectNetwork(networkID)
		}
	}

	return nil
}

//returns a drouter IP that is on some same network as provided container
func (dr *DistributedRouter) getContainerPathIP(ch *netlink.Handle) (net.IP, error) {
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
			return src[0].Src, nil
		}
	}

	return nil, fmt.Errorf("No direct connection to container.")
}
