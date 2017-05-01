package drouter

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"

	log "github.com/Sirupsen/logrus"
	"github.com/TrilliumIT/iputil"
	dockertypes "github.com/docker/engine-api/types"
	"github.com/vishvananda/netlink"
	"golang.org/x/net/context"
)

const (
	gatewayOffset = 100
)

type container struct {
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

	//log.Infof("State: %v", cjson.State.Running)
	if !cjson.State.Running {
		log.WithFields(log.Fields{"id": id}).Debug("Container not running.")
		return &container{
			handle: nil,
			id:     cjson.ID,
			log: log.WithFields(log.Fields{
				"container": map[string]string{
					"Name": cjson.Name,
					"ID":   cjson.ID,
				}}),
		}, nil
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
		return c.replaceGateway()
	}

	var routeAddWG sync.WaitGroup
	//Loop through all static routes, ensure each one is installed in the container
	//add routes for all the static routes
	for i, r := range staticRoutes {
		if i == 0 {
			c.log.Info("Syncing static routes")
		}
		routeAddWG.Add(1)
		sr := r
		go func() {
			defer routeAddWG.Done()
			c.addRoute(sr)
		}()
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
		routeAddWG.Add(1)
		cont := c
		nr := r
		go func() {
			defer routeAddWG.Done()
			cont.addRoute(nr.Dst)
		}()
	}

	routeAddWG.Wait()
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

	//Test checks for this message. Change with caution.
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

	gws, err := c.getPathIPs()
	if err != nil {
		// this may not be an error, expected when run against a shut down container.
		c.log.Debug("Failed to get path IPs", err)
		return
	}

	var altgw net.IP
	for _, gw := range gws {
		if via != nil && !via.Contains(gw) {
			c.log.WithFields(log.Fields{
				"AltGateway": gw,
			}).Debug("Alternate gateway found.")
			altgw = gw
			break
		}
	}

	//get all drouter ips
	ips, err := netlink.AddrList(nil, netlink.FAMILY_ALL)
	if err != nil {
		logError("Failed to get drouter ip addresses.", err)
		return
	}

	//get all container routes
	routes, err := c.getRoutes()
	if err != nil {
		c.logError("Failed to get container route table.", err)
		return
	}

	for _, r := range routes {
		if r.Dst != nil && to != nil && !iputil.SubnetContainsSubnet(to, r.Dst) {
			continue
		}

		for _, ipaddr := range ips {
			if !r.Gw.Equal(ipaddr.IP) || (via != nil && !via.Contains(r.Gw)) {
				continue
			}
			c.log.WithFields(log.Fields{
				"Destination": r.Dst,
				"Gateway":     r.Gw,
			}).Debug("Deleting route from container")
			err := c.handle.RouteDel(&r)
			// no such process just means the route was already deleted
			// this happens on shutdown cause there are multiple paths that might try to delete the route.
			if err != nil && err.Error() != "no such process" {
				c.log.WithFields(log.Fields{
					"Destination": r.Dst,
					"Gateway":     r.Gw,
					"Error":       err,
				}).Error("Failed to delete route from container")
				continue
			}
			if altgw == nil && r.Dst == nil {
				err = c.offsetGateways(gatewayOffset * -1)
				if err != nil {
					c.logError("Failed to offset gateway", err)
				}
			}
			if altgw != nil {
				c.addRouteVia(r.Dst, altgw)
			}
		}
	}
}

//sets the provided gateway's container to a connected drouter address
func (c *container) replaceGateway() error {
	c.log.Debug("Replacing gateway.")
	gateway, err := c.getPathIP()
	if err != nil {
		c.logError("Failed to get path IP", err)
		return err
	}
	return c.setGatewayTo(gateway)
}

//sets the provided container's default route to the provided gateway
func (c *container) setGatewayTo(gateway net.IP) error {
	c.log.WithFields(log.Fields{
		"Gateway": gateway,
	}).Debug("Setting gateway.")

	err := c.offsetGateways(gatewayOffset)
	if err != nil {
		c.logError("Failed to offset gateways", err)
	}

	if gateway == nil || gateway.Equal(net.IP{}) {
		c.log.WithFields(log.Fields{
			"Gateway": gateway,
		}).Debug("Gateway is nil, doing nothing.")
		return nil
	}

	route := &netlink.Route{Dst: nil, Gw: gateway}
	err = c.handle.RouteAdd(route)
	if err != nil {
		c.log.WithFields(log.Fields{
			"Gateway": gateway,
			"Error":   err,
		}).Debug("Failed to add container default route.")
		return err
	}
	c.log.WithFields(log.Fields{
		"Gateway": gateway,
	}).Info("Gateway successfully set.")

	return nil
}

// called during a network connect event
func (c *container) connectEvent(drn *network) error {
	c.log.WithFields(log.Fields{
		"network": drn,
	}).Debug("Container connect event.")
	if !drn.isConnected() {
		//connect drouter to the network that the container just connected to
		c.log.WithFields(log.Fields{
			"network": drn,
		}).Debug("Network not connected, connecting asynchronously.")
		drn.connect()
		//return now because the routeEvent will trigger routes to be installed in this container
		return nil
	}

	//let's push our routes into this new container
	if containerGateway {
		c.log.WithFields(log.Fields{
			"network": drn,
		}).Debug("Asynchronously replacing container gateway.")
		return c.replaceGateway()
	}

	for _, subnet := range drn.Subnets {
		c.delRoutes(subnet)
	}

	c.log.Debug("Asynchronously adding all routes to container.")
	err := c.addAllRoutes()
	if err != nil {
		log.WithError(err).Error("Error adding all routes to container")
		return err
	}

	err = c.publishModifyRoute(drn, addRoute)
	if err != nil {
		log.WithError(err).Error("Error publishing container specific routes")
	}
	return err
}

func (c *container) publishModifyRoute(drn *network, action bool) error {
	if len(transitNetName) <= 0 {
		return nil
	}
	c.log.Debug("Publishing container /32 routes")
	addrs, err := c.getAddrs()
	if err != nil {
		c.logError("Failed to list container addresses.", err)
		return err
	}
	for _, addr := range addrs {
		for _, subnet := range drn.Subnets {
			if subnet.Contains(addr.IP) {
				n := &net.IPNet{
					IP:   addr.IP,
					Mask: net.CIDRMask(len(addr.IPNet.Mask), len(addr.IPNet.Mask)), // Make the mask all 1's
				}
				rs.ModifyRoute(n, action)
			}
		}
	}
	return nil
}

// called when we detect a container has disconnected from a drouter network
func (c *container) disconnectEvent(drn *network) error {
	c.log.WithFields(log.Fields{
		"network": drn,
	}).Debug("Container disconnect event.")
	for _, subnet := range drn.Subnets {
		c.delRoutesVia(nil, subnet)
	}

	err := c.publishModifyRoute(drn, delRoute)
	if err != nil {
		log.WithError(err).Error("Error publishing container specific routes")
	}

	if pIP, err := c.getPathIP(); err == nil {
		c.log.WithFields(log.Fields{
			"PathIP": pIP,
		}).Debug("Found a remaining path IP, adding all routes.")
		err = c.addAllRoutes()
		if err != nil {
			c.logError("Failed to add all routes", err)
		}
	}

	if aggressive || drn.ID == transitNetID {
		return nil
	}

	//loop through all the containers
	dockerContainers, err := dockerClient.ContainerList(context.Background(), dockertypes.ContainerListOptions{})
	if err != nil {
		c.logError("Failed to get container list", err)
		return err
	}
	for _, dc := range dockerContainers {
		if dc.HostConfig.NetworkMode == hostNetworkMode {
			continue
		}
		if dc.ID == selfContainerID {
			continue
		}

		for n := range dc.NetworkSettings.Networks {
			if n == drn.Name {
				c.log.WithFields(log.Fields{
					"net-container": map[string]interface{}{
						"Names": dc.Names,
						"ID":    dc.ID,
					},
				}).Debug("Network still contains net-container")
				return nil
			}
		}
	}

	select {
	case <-stopChan:
		c.log.WithFields(log.Fields{
			"network": drn,
		}).Debug("Shutdown detected.")
	default:
		c.log.WithFields(log.Fields{
			"network": drn,
		}).Debug("Disconnecting from network.")
		drn.disconnect()
	}

	return nil
}

func getPathIPs(addrs ...netlink.Addr) ([]net.IP, error) {
	var ips []net.IP
	for _, addr := range addrs {
		if addr.Label == "lo" {
			continue
		}

		log.WithFields(log.Fields{
			"IP": addr.IP,
		}).Debug("Getting routes to IP")
		srcRoutes, err := netlink.RouteGet(addr.IP)
		if err != nil {
			log.WithFields(log.Fields{
				"IP":    addr.IP,
				"Error": err,
			}).Error("Failed to get routes to IP")
			continue
		}
		for _, srcRoute := range srcRoutes {
			if srcRoute.Gw == nil {
				log.WithFields(log.Fields{
					"IP":  addr.IP,
					"Src": srcRoute.Src,
				}).Debug("Found direct route to IP from Src.")
				ips = append(ips, srcRoute.Src)
			}
		}
	}

	return ips, nil
}

func (c *container) getAddrs() ([]netlink.Addr, error) {
	c.log.Debug("Getting addresses")
	if c.handle == nil {
		err := fmt.Errorf("No namespace handle for container, is it running?")
		// not necessarily an error. Happens when container stops in non-aggressive mode
		c.log.Debug("No namespace handle for container", err)
		return nil, err
	}

	return c.handle.AddrList(nil, netlink.FAMILY_V4)
}

//returns a drouter IP that is on some same network as provided container
func (c *container) getPathIPs() ([]net.IP, error) {
	c.log.Debug("Getting path IPs")

	addrs, err := c.getAddrs()
	if err != nil {
		c.logError("Failed to list container addresses.", err)
		return nil, err
	}

	return getPathIPs(addrs...)
}

func (c *container) getPathIP() (net.IP, error) {
	c.log.Debug("Getting path IP")
	// TODO: This needs to accept a family
	ips, err := c.getPathIPs()
	if err != nil {
		// this may not be an error, expected when run against a shut down container.
		c.log.Debug("Failed to get path IPs", err)
		return nil, err
	}

	if len(ips) == 0 {
		err = fmt.Errorf("No direct connection to container.")
		c.logError("No direct routes to container", err)
		return nil, err
	}

	c.log.WithFields(log.Fields{
		"IP": ips[0],
	}).Debug("Returning Path IP")
	return ips[0], nil
}

func (c *container) getRoutes() ([]netlink.Route, error) {
	c.log.Debug("Getting container routes.")
	if c.handle != nil {
		return c.handle.RouteList(nil, netlink.FAMILY_ALL)
	}

	return make([]netlink.Route, 0), nil
}

func (c *container) offsetGateways(offset int) error {
	c.log.WithFields(log.Fields{
		"Offset": offset,
	}).Debug("Offsetting container gateways")
	routes, err := c.getRoutes()
	if err != nil {
		c.logError("Failed to get container routes", err)
		return err
	}

	for _, r := range routes {
		if r.Dst != nil {
			// Not the container gateway
			continue
		}

		c.log.WithFields(log.Fields{
			"Route": r,
		}).Debug("Removing existing default route")
		err := c.handle.RouteDel(&r)
		if err != nil {
			return err
		}

		if r.Priority+offset < 0 {
			r.Priority = 0
		} else {
			r.Priority += offset
		}

		c.log.WithFields(log.Fields{
			"Route": r,
		}).Debug("Adding default route with new priority")
		err = c.handle.RouteAdd(&r)
		if err != nil {
			return err
		}
	}
	return nil
}
