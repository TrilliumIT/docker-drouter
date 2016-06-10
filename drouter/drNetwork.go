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
	connected bool
	subnets   []*net.IPNet
}

func (dr *DistributedRouter) drNetworkConnect(n *dockertypes.NetworkResource) error {
	log.Debugf("Connecting to network: %v", n.Name)

	endpointSettings := &dockernetworks.EndpointSettings{}
	subnets := make([]*net.IPNet, len(n.IPAM.Config))

	//select drouter IP for network
	if dr.ipOffset != 0 {
		var ip net.IP
		log.Debugf("ip-offset configured to: %v", dr.ipOffset)
		for i, ipamconfig := range n.IPAM.Config {
			_, subnet, err := net.ParseCIDR(ipamconfig.Subnet)
			log.Debugf("Adding subnet %v", subnet)
			subnets[i] = subnet
			if err != nil {
				return err
			}
			if dr.ipOffset > 0 {
				ip = netaddr.IPAdd(subnet.IP, dr.ipOffset)
			} else {
				last := ipaddress.LastAddress(subnet)
				ip = netaddr.IPAdd(last, dr.ipOffset)
			}
			if endpointSettings.IPAddress == "" {
				endpointSettings.IPAddress = ip.String()
				endpointSettings.IPAMConfig =&dockernetworks.EndpointIPAMConfig{
					IPv4Address: ip.String(),
				}
			} else {
				endpointSettings.Aliases = append(endpointSettings.Aliases, ip.String())
			}
			log.Debugf("Adding IP %v to network %v", ip, n.Name)
		}
	}

	//connect to network
	err := dr.dc.NetworkConnect(context.Background(), n.ID, dr.selfContainer.ID, endpointSettings)
	if err != nil {
		return err
	}

	dr.networks[n.ID] = &drNetwork{
		connected: true,
		subnets: subnets,
	}

	//refresh our self
	dr.updateSelfContainer()

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

	//if we managed drouters default route
	if dr.defaultRoute != nil && !dr.defaultRoute.Equal(net.IP{}) {
		//ensure default route for drouter is correct
		err = dr.setDefaultRoute()
		if err != nil {
			return err
		}
	}

	return nil
}

func (dr *DistributedRouter) drNetworkDisconnect(networkid string) error {
	log.Debugf("Attempting to remove network: %v", networkid)

	err := dr.dc.NetworkDisconnect(context.Background(), networkid, dr.selfContainer.ID, true)
	if err != nil {
		return err
	}
	log.Debugf("Disconnected from network: %v", networkid)

	if _, ok := dr.networks[networkid]; !ok {
		return nil
	} 

	dr.networks[networkid].connected = false

	return nil
}

func (dr *DistributedRouter) syncNetworks() error {
	log.Debug("Syncing networks from docker.")

	syncRoutes := false
	//get all networks from docker
	nets, err := dr.dc.NetworkList(context.Background(), dockertypes.NetworkListOptions{ Filters: dockerfilters.NewArgs(), })
	if err != nil {
		log.Error("Error getting network list")
		return err
	}

	//leave and remove invalid or missing networks
	for id, drn := range dr.networks {
		known := false
		for _, network := range nets {
			if id == network.ID {
				known = true
				break
			}
		}

		if !known {
			syncRoutes = true
			err := dr.drNetworkDisconnect(id)
			if err != nil {
				log.Error(err)
				continue
			}
			delete(dr.networks, id)
		}
	}

	//join and add new networks
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

		if network.Name == dr.transitNet {
			log.Infof("Transit net %v found, and detected as ID: %v", dr.transitNet, network.ID)
			dr.transitNetID = network.ID
			if len(network.Options["gateway"]) > 0 && !dr.localGateway {
				log.Debugf("Gateway option detected on transit net. Saving for default route.")
				dr.defaultRoute = net.ParseIP(network.Options["gateway"])
			}
		}

		connected := false
		if n, ok := dr.networks[network.ID]; ok {
			connected = n.connected
		}


		if drouter {
			if (dr.aggressive || network.ID == dr.transitNet) && !connected {
				err := dr.drNetworkConnect(&network)
				if err != nil {
					log.Errorf("Error joining network: %v", network)
					return err
				}
				syncRoutes = true
			}
		}
	}

	if syncRoutes {
		err := dr.syncAllRoutes()
		if err != nil {
			log.Error("Failed to sync container routes after syncNetworks reported new connections.")
			return err
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
