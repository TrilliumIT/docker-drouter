package drouter

import (
  "net"
  log "github.com/Sirupsen/logrus"
	dockertypes "github.com/docker/engine-api/types"
	dockernetworks "github.com/docker/engine-api/types/network"
	"golang.org/x/net/context"
	"github.com/vishvananda/netlink"
	"github.com/ziutek/utils/netaddr"
	"github.com/llimllib/ipaddress"
)

type drNetwork struct {
	id        string
	connected bool
	drouter   bool
	ipam      dockernetworks.IPAM
	ip        net.IP
	subnet    *net.IPNet
	gateway   net.IP
}

func (dr *DistributedRouter) drNetworkConnect(n *dockertypes.NetworkResource) error {
	endpointSettings := &dockernetworks.EndpointSettings{}
	var subnet net.IPNet
	var ip net.IP

	//select drouter IP for network
	if dr.ipOffset != 0 {
		for _, ipamconfig := range n.IPAM.Config {
			log.Debugf("ip-offset configured")
			_, subnet, err := net.ParseCIDR(ipamconfig.Subnet)
			if err != nil {
				return err
			}
			if dr.ipOffset > 0 {
				ip = netaddr.IPAdd(subnet.IP, dr.ipOffset)
			} else {
				last := ipaddress.LastAddress(subnet)
				ip = netaddr.IPAdd(last, dr.ipOffset)
			}
			log.Debugf("Setting IP to %v", ip)
			if endpointSettings.IPAddress == "" {
				endpointSettings.IPAddress = ip.String()
				endpointSettings.IPAMConfig =&dockernetworks.EndpointIPAMConfig{
					IPv4Address: ip.String(),
				}
			} else {
				endpointSettings.Aliases = append(endpointSettings.Aliases, ip.String())
			}
		}
	}

	//connect to network
	err := dr.dc.NetworkConnect(context.Background(), n.ID, dr.selfContainer.ID, endpointSettings)
	if err != nil {
		return err
	}

	//refresh our self
	dr.updateSelfContainer()
	
	//get the gateway for our new network
	gateway := net.ParseIP(dr.selfContainer.NetworkSettings.Networks[n.ID].Gateway)

	dr.networks[n.ID] = &drNetwork{
		connected: true,
		drouter: true,
		ip: ip,
		subnet: &subnet,
		gateway: gateway,
	}

	//loop through all containers, and and add drouter route for vxlans
	if !dr.localGateway && len(dr.summaryNets) == 0 {
	}
	
	//add routes to host for drouter if localShortcut is enabled
	if dr.localShortcut {
		for _, ipamconfig := range n.IPAM.Config {
			_, dst, err := net.ParseCIDR(ipamconfig.Subnet)
			if err != nil {
				return err
			}
			route := &netlink.Route{
				LinkIndex: dr.p2p.hostLinkIndex,
				Gw: dr.p2p.selfIP,
				Dst: dst,
			}
			err = dr.hostNamespace.RouteAdd(route)
			if err != nil {
				return err
			}
		}
	}

	//remove all default routes after joining network
	routes, err := dr.selfNamespace.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return err
	}
	for _, r := range routes {
		if r.Dst != nil {
			continue
		}

		//TODO: test that inteded default is already default, don't remove if so
		log.Debugf("Remove default route: %v", r)
		err = dr.selfNamespace.RouteDel(&r)
		if err != nil {
			return err
		}
	}

	//add intended default route, if it's not set necessary

	return nil
}

func (dr *DistributedRouter) drNetworkDisconnect(networkid string) error {
	err := dr.dc.NetworkDisconnect(context.Background(), networkid, dr.selfContainer.ID, true)
	if err != nil {
		return err
	}
	dr.networks[networkid].connected = false

	//loop through all containers, and and remove drouter route for vxlans

	return nil
}
