package drouter

import (
	"time"
	"strconv"
  "net"
  log "github.com/Sirupsen/logrus"
	dockertypes "github.com/docker/engine-api/types"
	dockerfilters "github.com/docker/engine-api/types/filters"
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

	//add this network to all container's routing tables
	if !dr.localGateway && len(dr.summaryNets) == 0 {
		err = dr.addNetworkRoutes(dr.networks[n.ID])
		if err != nil {
			return err
		}
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

	//ensure default route for drouter is correct
	err = dr.setDefaultRoute()
	if err != nil {
		return err
	}

	return nil
}

func (dr *DistributedRouter) drNetworkDisconnect(networkid string) error {
	err := dr.dc.NetworkDisconnect(context.Background(), networkid, dr.selfContainer.ID, true)
	if err != nil {
		return err
	}
	dr.networks[networkid].connected = false

	//remove network from all container's routing tables
	if !dr.localGateway && len(dr.summaryNets) == 0 {
		err = dr.delNetworkRoutes(dr.networks[networkid])
		if err != nil {
			return err
		}
	}

	return nil
}

func (dr *DistributedRouter) syncNetworks() error {
	//get all networks from docker
	nets, err := dr.dc.NetworkList(context.Background(), dockertypes.NetworkListOptions{ Filters: dockerfilters.NewArgs(), })
	if err != nil {
		log.Error("Error getting network list")
		return err
	}
	for _, network := range nets {
		drouter_str := network.Options["drouter"]
		drouter := false
		if drouter_str != "" {
			drouter, err = strconv.ParseBool(drouter_str) 
			if err != nil {
				log.Errorf("Error parsing drouter option: %v", drouter_str)
				return err
			}
		} 

		if drouter {
			dr.networks[network.ID].drouter = true
			if (dr.aggressive || network.ID == dr.transitNet) && !dr.networks[network.ID].connected {
				log.Debugf("Joining Net: %+v", network)
				err := dr.drNetworkConnect(&network)
				if err != nil {
					log.Errorf("Error joining network: %v", network)
					return err
				}
			}
		} else {
			dr.networks[network.ID].drouter = false
			if dr.networks[network.ID].connected {
				log.Debugf("Leaving Net: %+v", network)
				err := dr.drNetworkDisconnect(network.ID)
				if err != nil {
					log.Errorf("Error leaving network: %v", network)
					return err
				}
			}
		}
	}
	return nil
}

func (dr *DistributedRouter) watchNetworks() {
	log.Info("Watching Networks")
	for {
		//TODO: make this timeout a variable
		time.Sleep(5 * time.Second)

		err := dr.syncNetworks()
		if err != nil {
			log.Error(err)
		}
	}
}
