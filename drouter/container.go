package drouter

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	log "github.com/Sirupsen/logrus"
	"github.com/TrilliumIT/iputil"
	dockertypes "github.com/docker/engine-api/types"
	"github.com/vishvananda/netlink"
	"golang.org/x/net/context"
)

const (
	GATEWAY_OFFSET = 100
)

type container struct {
	name   string
	id     string
	log    *log.Entry
	handle *netlink.Handle
}

func newContainerFromID(id string) (*container, error) {
	log.WithFields(log.Fields{
		"ID": id,
	}).Debug("Creating container object.")
	cjson, err := dockerClient.ContainerInspect(context.Background(), id)
	if err != nil {
		log.WithFields(log.Fields{"id": id}).Error("Failed to inspect container.")
		return nil, err
	}
	log.WithFields(log.Fields{"id": id}).Debug("Inspected container.")

	if !cjson.State.Running {
		log.WithFields(log.Fields{"id": id}).Debug("Container not running.")
		return &container{}, nil
	}

	ch, err := netlinkHandleFromPid(cjson.State.Pid)
	if err != nil {
		log.WithFields(log.Fields{
			"container": map[string]string{
				"ID":  id,
				"Pid": strconv.Itoa(cjson.State.Pid),
			},
			"Error": err,
		}).Error("Failed to get netlink handle")
		return nil, err
	}

	log.WithFields(log.Fields{"id": id}).Debug("Returning container object")
	return &container{
		handle: ch,
		name:   cjson.Name,
		id:     cjson.ID,
		log: log.WithFields(log.Fields{
			"container": map[string]string{
				"Name": cjson.Name,
				"ID":   cjson.ID,
			}}),
	}, nil
}

func (c *container) logError(msg string, err error) {
	c.log.WithFields(log.Fields{"Error": err}).Error(msg)
}

//adds all known routes for provided container
func (c *container) addAllRoutes() error {
	c.log.Debug("Adding all routes")
	if containerGateway {
		c.log.Debug("Calling replaceGateway")
		c.replaceGateway()
		return nil
	}

	//Loop through all static routes, ensure each one is installed in the container
	//add routes for all the static routes
	for i, sr := range staticRoutes {
		if i == 0 {
			c.log.Info("Syncing static routes")
		}
		go c.addRoute(sr)
	}

	//Loop through all discovered networks, ensure each one is installed in the container
	//Unless it is covered by a static route already
	routes, err := netlink.RouteList(nil, netlink.FAMILY_ALL)
	if err != nil {
		logError("Failed to get my routes.", err)
	}

Subnets:
	for i, r := range routes {
		if i == 0 {
			c.log.Info("Syncing discovered routes.")
		}
		if r.Gw != nil {
			continue
		}
		if hostShortcut {
			if iputil.SubnetEqualSubnet(r.Dst, iputil.NetworkID(p2p.network)) {
				c.log.WithFields(log.Fields{
					"Destination": r.Dst,
					"p2pNet":      p2p.network,
				}).Debug("Skipping route to p2p network.")
				continue
			}
		}
		for _, sr := range staticRoutes {
			if iputil.SubnetContainsSubnet(sr, r.Dst) {
				c.log.WithFields(log.Fields{
					"Destination": r.Dst,
					"Static":      sr,
				}).Debug("Skipping route covered by static.")
				continue Subnets
			}
		}

		croutes, err := c.getRoutes()
		if err != nil {
			c.logError("Failed to get container routes", err)
			return err
		}

		for _, cr := range croutes {
			if cr.Gw != nil {
				continue
			}
			if iputil.SubnetContainsSubnet(cr.Dst, r.Dst) {
				c.log.WithFields(log.Fields{
					"Destination": r.Dst,
					"Direct":      cr.Dst,
				}).Debug("Skipping already direct route.")
				continue Subnets
			}
		}

		c.log.WithFields(log.Fields{
			"Destination": r.Dst,
		}).Debug("Adding route")
		go c.addRoute(r.Dst)
	}

	return nil
}

func (c *container) addRoute(sn *net.IPNet) {
	c.log.WithFields(log.Fields{
		"Subnet": sn,
	}).Debug("Adding route to container.")
	gateway, err := c.getPathIP()
	if err != nil {
		c.logError("Failed to get path IP", err)
		return
	}
	c.addRouteVia(sn, gateway)
}

func (c *container) addRouteVia(sn *net.IPNet, gateway net.IP) {
	c.log.WithFields(log.Fields{
		"Subnet":  sn,
		"Gateway": gateway,
	}).Debug("Adding route to container via gateway.")
	if sn != nil && (sn.IP.To4() == nil) != (gateway.To4() == nil) {
		// Dst is a different IP family
		c.log.WithFields(log.Fields{
			"Subnet":  sn,
			"Gateway": gateway,
		}).Debug("Gateway and Subnet Families don't match")
		return
	}

	route := &netlink.Route{
		Dst: sn,
		Gw:  gateway,
	}

	c.log.WithFields(log.Fields{
		"Subnet":  sn,
		"Gateway": gateway,
	}).Info("Adding route to container.")
	err := c.handle.RouteAdd(route)

	// all went well, return
	if err == nil {
		return
	}

	// something other than file exists failed
	if !strings.Contains(err.Error(), "file exists") {
		c.log.WithFields(log.Fields{
			"Subnet":  sn,
			"Gateway": gateway,
			"Error":   err,
		}).Error("Failed to Add route to container.")
		return
	}

	// got file exists, delete and try again
	routes, err := c.handle.RouteGet(sn.IP)
	if err != nil {
		c.log.WithFields(log.Fields{
			"IP":    sn.IP,
			"Error": err,
		}).Error("Failed to get route to IP.")
		return
	}
	for _, r := range routes {
		if r.Gw.Equal(gateway) || r.Gw == nil {
			c.log.WithFields(log.Fields{
				"Subnet":           sn,
				"Gateway":          gateway,
				"Existing Gateway": r.Gw,
			}).Debug("Route gateway is already correct")
			return
		}
		err := c.handle.RouteDel(&netlink.Route{Dst: sn, Gw: r.Gw})
		if err != nil {
			c.log.WithFields(log.Fields{
				"Subnet":  sn,
				"Gateway": r.Gw,
				"Error":   err,
			}).Error("Failed to delete route")
			return
		}
		c.log.WithFields(log.Fields{
			"Subnet":  sn,
			"Gateway": r.Gw,
		}).Debug("Deleted route")
	}

	c.log.WithFields(log.Fields{
		"Subnet":  sn,
		"Gateway": gateway,
	}).Debug("Retrying add route")
	c.addRoute(sn)
}

func (c *container) delRoutes(to *net.IPNet) {
	c.log.WithFields(log.Fields{
		"Subnet": to,
	}).Debug("Deleting routes to subnet.")
	c.delRoutesVia(to, nil)
}

func (c *container) delRoutesVia(to, via *net.IPNet) {
	c.log.WithFields(log.Fields{
		"Subnet": to,
		"Via":    via,
	}).Debug("Deleting routes to subnet via gateways in Via.")
	//get all container routes
	routes, err := c.getRoutes()
	if err != nil {
		c.logError("Failed to get container route table.", err)
		return
	}

	//get all drouter ips
	ips, err := netlink.AddrList(nil, netlink.FAMILY_ALL)
	if err != nil {
		logError("Failed to get drouter ip addresses.", err)
		return
	}

	gws, err := c.getPathIPs()
	if err != nil {
		c.logError("Failed to get path IPs", err)
		return
	}

	var altgw net.IP = nil
	for _, gw := range gws {
		if via != nil && !via.Contains(gw) {
			c.log.WithFields(log.Fields{
				"AltGateway": gw,
			}).Debug("Alternate gateway found.")
			altgw = gw
			break
		}
	}

	for _, r := range routes {
		if r.Dst != nil && to != nil && !iputil.SubnetContainsSubnet(to, r.Dst) {
			continue
		}

		for _, ipaddr := range ips {
			if !r.Gw.Equal(ipaddr.IP) {
				continue
			}
			if via != nil && !via.Contains(r.Gw) {
				continue
			}
			c.log.WithFields(log.Fields{
				"Destination": r.Dst,
				"Gateway":     r.Gw,
			}).Debug("Deleting route from container")
			err := c.handle.RouteDel(&r)
			if err != nil {
				c.log.WithFields(log.Fields{
					"Destination": r.Dst,
					"Gateway":     r.Gw,
					"Error":       err,
				}).Error("Failed to delete route from container")
				continue
			}
			if altgw == nil && r.Dst == nil {
				c.offsetGateways(GATEWAY_OFFSET * -1)
			}
			if altgw != nil {
				c.addRouteVia(r.Dst, altgw)
			}
		}
	}
}

//sets the provided gateway's container to a connected drouter address
func (c *container) replaceGateway() error {
	gateway, err := c.getPathIP()
	if err != nil {
		log.Error("Failed to get path IP when replacing the gateway.")
		return err
	}
	return c.setGatewayTo(gateway)
}

//sets the provided container's default route to the provided gateway
func (c *container) setGatewayTo(gateway net.IP) error {
	c.offsetGateways(GATEWAY_OFFSET)

	if gateway == nil || gateway.Equal(net.IP{}) {
		return nil
	}

	route := &netlink.Route{Dst: nil, Gw: gateway}
	err := c.handle.RouteAdd(route)
	if err != nil {
		return err
	}
	log.Info("Default route changed to: %v", route)

	return nil
}

// called during a network connect event
func (c *container) connectEvent(drn *network) error {
	if !drn.isConnected() {
		//connect drouter to the network that the container just connected to
		connectWG.Add(1)
		go func() {
			defer connectWG.Done()
			drn.connect()
		}()
		//return now because the routeEvent will trigger routes to be installed in this container
		return nil
	}

	//let's push our routes into this new container
	if containerGateway {
		go c.replaceGateway()
		return nil
	}

	for _, ic := range drn.IPAM.Config {
		_, subnet, err := net.ParseCIDR(ic.Subnet)
		if err != nil {
			log.Error(err)
			continue
		}
		c.delRoutes(subnet)
	}

	go c.addAllRoutes()
	return nil
}

// called when we detect a container has disconnected from a drouter network
func (c *container) disconnectEvent(drn *network) error {
	for _, ic := range drn.IPAM.Config {
		subnet, err := netlink.ParseIPNet(ic.Subnet)
		if err != nil {
			log.Errorf("Failed to parse ipam config subnet: %v", ic.Subnet)
			log.Error(err)
			continue
		}
		c.delRoutesVia(nil, subnet)
	}

	if _, err := c.getPathIP(); err == nil {
		c.addAllRoutes()
	}

	if aggressive {
		return nil
	}

	//if not aggressive mode, then we disconnect from the network if this is the last connected container
	connectWG.Wait()

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

	select {
	case _ = <-stopChan:
	default:
		go drn.disconnect()
	}

	return nil
}

//returns a drouter IP that is on some same network as provided container
func (c *container) getPathIPs() (ips []net.IP, err error) {
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
				ips = append(ips, srcRoute.Src)
			}
		}
	}

	return
}

func (c *container) getPathIP() (net.IP, error) {
	// TODO: This needs to accept a family
	ips, err := c.getPathIPs()
	if err != nil {
		return nil, err
	}

	if len(ips) == 0 {
		return nil, fmt.Errorf("No direct connection to container.")
	}

	return ips[0], nil
}

func (c *container) getRoutes() ([]netlink.Route, error) {
	if c.handle != nil {
		return c.handle.RouteList(nil, netlink.FAMILY_ALL)
	}

	return make([]netlink.Route, 0), nil
}

func (c *container) offsetGateways(offset int) error {
	routes, err := c.getRoutes()
	if err != nil {
		return err
	}

	for _, r := range routes {
		if r.Dst != nil {
			// Not the container gateway
			continue
		}

		log.Debugf("Remove existing default route: %v", r)
		err := c.handle.RouteDel(&r)
		if err != nil {
			return err
		}

		if r.Priority+offset < 0 {
			r.Priority = 0
		} else {
			r.Priority += offset
		}

		err = c.handle.RouteAdd(&r)
		if err != nil {
			return err
		}
	}
	return nil
}
