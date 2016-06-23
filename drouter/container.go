package drouter

import (
	"fmt"
	log "github.com/Sirupsen/logrus"
	dockertypes "github.com/docker/engine-api/types"
	"github.com/vishvananda/netlink"
	"golang.org/x/net/context"
	"net"
	"strings"
)

type container struct {
	handle *netlink.Handle
}

func newContainerFromID(id string) (*container, error) {
	cjson, err := dockerClient.ContainerInspect(context.Background(), id)
	if err != nil {
		return nil, err
	}
	if !cjson.State.Running {
		return &container{}, nil
	}

	ch, err := netlinkHandleFromPid(cjson.State.Pid)
	if err != nil {
		return nil, err
	}

	return &container{handle: ch}, nil
}

//adds all known routes for provided container
func (c *container) addAllRoutes() error {
	//Loop through all static routes, ensure each one is installed in the container
	log.Info("Syncing static routes.")
	//add routes for all the static routes
	for _, sr := range staticRoutes {
		go c.addRoute(sr)
	}

	//Loop through all discovered networks, ensure each one is installed in the container
	//Unless it is covered by a static route already
	log.Info("Syncing discovered routes.")
	routes, err := netlink.RouteList(nil, netlink.FAMILY_ALL)
	if err != nil {
		log.Error("Failed to get my routes.")
		log.Error(err)
	}

Subnets:
	for _, r := range routes {
		if r.Gw != nil {
			continue
		}
		if localShortcut {
			if subnetEqualSubnet(r.Dst, networkID(p2p.network)) {
				continue
			}
		}
		for _, sr := range staticRoutes {
			if subnetContainsSubnet(sr, r.Dst) {
				log.Debugf("Skipping route %v covered by %v.", r.Dst, sr)
				break Subnets
			}
		}
		//TODO: filter routes that container is already connected to
		go c.addRoute(r.Dst)
	}

	return nil
}

func (c *container) addRoute(sn *net.IPNet) {
	gateway, err := c.getPathIP()
	if err != nil {
		log.Error(err)
		return
	}

	if (sn.IP.To4() == nil) != (gateway.To4() == nil) {
		// Dst is a different IP family
		return
	}

	route := &netlink.Route{
		Dst: sn,
		Gw:  gateway,
	}

	log.Infof("Adding route to %v via %v.", sn, gateway)
	err = c.handle.RouteAdd(route)
	if err != nil {
		if !strings.Contains(err.Error(), "file exists") {
			log.Error(err)
			return
		}
		routes, err2 := c.handle.RouteGet(sn.IP)
		if err2 != nil {
			log.Errorf("Failed to get container routes to: %v.", sn.IP)
			log.Error(err)
			return
		}
		for _, r := range routes {
			if r.Gw.Equal(gateway) || r.Gw == nil {
				return
			}
			err := c.handle.RouteDel(&netlink.Route{Dst: sn, Gw: r.Gw})
			if err != nil {
				log.Errorf("Failed to delete route to %v via %v.", sn, r.Gw)
				log.Error(err)
				return
			}
		}
		c.addRoute(sn)
	}
}

func (c *container) delRoutes(to, via *net.IPNet) {
	//get all container routes
	routes, err := c.getRoutes()
	if err != nil {
		log.Error("Failed to get container route table.")
		log.Error(err)
		return
	}

	//get all drouter ips
	ips, err := netlink.AddrList(nil, netlink.FAMILY_ALL)
	if err != nil {
		log.Error("Failed to get drouter ip addresses.")
		log.Error(err)
		return
	}

	for _, r := range routes {
		if r.Dst == nil {
			continue
		}
		if !subnetContainsSubnet(to, r.Dst) {
			continue
		}

		for _, ipaddr := range ips {
			if !r.Gw.Equal(ipaddr.IP) {
				continue
			}
			if !via.Contains(r.Gw) {
				continue
			}
			err := c.handle.RouteDel(&r)
			if err != nil {
				log.Errorf("Failed to delete container route to %v via %v", r.Dst, r.Gw)
				continue
			}
		}
	}
}

//sets the provided container's default route to the provided gateway
func (c *container) replaceGateway(gateway net.IP) error {
	log.Debugf("container.replaceGateway(%v)", gateway)

	var defr *netlink.Route
	//replace containers default gateway with drouter
	routes, err := c.getRoutes()
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
	err = c.handle.RouteDel(defr)
	if err != nil {
		return err
	}

	if gateway == nil || gateway.Equal(net.IP{}) {
		return nil
	}

	defr.Gw = gateway
	err = c.handle.RouteAdd(defr)
	if err != nil {
		return err
	}
	log.Debugf("Default route changed to: %v", defr)

	return nil
}

// called during a network connect event
func (c *container) connectEvent(drn *network) error {
	if !drn.isConnected() {
		drn.connect()
		//return now because the routeEvent will trigger routes to be installed in this container
		return nil
	}

	//let's push our routes into this new container
	gateway, err := c.getPathIP()
	if err != nil {
		log.Error("Failed to get container path IP.")
		return err
	}

	if localGateway {
		go c.replaceGateway(gateway)
	} else {
		go c.addAllRoutes()
	}
	return nil
}

// called during a network disconnect event
func (c *container) disconnectEvent(drn *network) error {

	for _, ic := range drn.IPAM.Config {
		subnet, err := netlink.ParseIPNet(ic.Subnet)
		if err != nil {
			log.Errorf("Failed to parse ipam config subnet: %v", ic.Subnet)
			log.Error(err)
			continue
		}
		c.delRoutes(makeGlobalNet(), subnet)
	}

	if _, err := c.getPathIP(); err == nil {
		c.addAllRoutes()
	}

	if aggressive {
		return nil
	}

	//if not aggressive mode, then we disconnect from the network if this is the last connected container

	//loop through all the containers
	dockerContainers, err := dockerClient.ContainerList(context.Background(), dockertypes.ContainerListOptions{})
	if err != nil {
		return err
	}
	for _, dc := range dockerContainers {
		if dc.HostConfig.NetworkMode == "host" {
			continue
		}
		if dc.ID == selfContainerID {
			continue
		}

		for n, _ := range dc.NetworkSettings.Networks {
			if n == drn.Name {
				// This netowork is still in use
				return nil
			}
		}
	}

	networkDisconnectWG.Add(len(drn.IPAM.Config))
	go drn.disconnect()

	return nil
}

//returns a drouter IP that is on some same network as provided container
func (c *container) getPathIP() (net.IP, error) {
	if c.handle == nil {
		return nil, fmt.Errorf("No namespace handle for container, is it running?")
	}

	addrs, err := c.handle.AddrList(nil, netlink.FAMILY_V4)
	if err != nil {
		log.Error("Failed to list container addresses.")
		return nil, err
	}

	for _, addr := range addrs {
		if addr.Label == "lo" {
			continue
		}

		log.Debugf("Getting my route to container IP: %v", addr.IP)
		srcRoutes, err := netlink.RouteGet(addr.IP)
		if err != nil {
			log.Error(err)
			continue
		}
		for _, srcRoute := range srcRoutes {
			if srcRoute.Gw == nil {
				return srcRoute.Src, nil
			}
		}
	}

	return nil, fmt.Errorf("No direct connection to container.")
}

func (c *container) getRoutes() ([]netlink.Route, error) {
	if c.handle != nil {
		return c.handle.RouteList(nil, netlink.FAMILY_ALL)
	}

	return make([]netlink.Route, 0), nil
}
