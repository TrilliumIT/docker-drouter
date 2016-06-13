package drouter

import (
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
	drouter   bool
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
				log.Error(err)
				continue
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
			subnets[i].IP = ip
			log.Debugf("Adding IP %v to network %v", ip, n.Name)
		}
	}

	//connect to network
	err := dr.dc.NetworkConnect(context.Background(), n.ID, dr.selfContainer.ID, endpointSettings)
	if err != nil {
		return err
	}

	dr.networks[n.ID].connected = true
	dr.networks[n.ID].subnets = subnets

	//refresh our self
	dr.updateSelfContainer()

	//add routes to host for drouter if localShortcut is enabled
	if dr.localShortcut {
		for _, sn := range subnets {
			route := &netlink.Route{
				LinkIndex: dr.p2p.hostLinkIndex,
				Gw: dr.p2p.selfIP,
				Dst: sn,
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

	contNetworks := make(map[string]struct{}, 0)
	//in aggressive mode, we don't care where containers are connected
	if !dr.aggressive {
		//Build a list of networks for which we have running contaners
		containers, err := dr.dc.ContainerList(context.Background(), dockertypes.ContainerListOptions{})
		if err != nil {
			return err
		}

		for _, c := range containers {
			//skip containers running with --net=host
			if c.HostConfig.NetworkMode == "host" {
				continue
			}

			//don't try to set routes for ourself
			if c.ID == dr.selfContainer.ID {
				continue
			}

			cjson, err := dr.dc.ContainerInspect(context.Background(), c.ID)
			if err != nil {
				log.Error(err)
				continue
			}

			for _, network := range cjson.NetworkSettings.Networks {
				contNetworks[network.NetworkID] = struct{}{}
			}
		}
	}

	syncRoutes := false
	//get all networks from docker
	nets, err := dr.dc.NetworkList(context.Background(), dockertypes.NetworkListOptions{ Filters: dockerfilters.NewArgs(), })
	if err != nil {
		log.Error("Error getting network list")
		return err
	}

	//leave and remove missing networks
	for id, _ := range dr.networks {
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
		//is this a drouter network
		drouter, err := isDRouterNetwork(&network)
		if err != nil {
			log.Error(err)
			continue
		}

		//is this specified as a transit net?
		tnet := false
		if network.Name == dr.transitNet {
			log.Infof("Transit net %v found, and detected as ID: %v", dr.transitNet, network.ID)
			if !drouter {
				log.Error("Transit net does not have the drouter option set, ignoring transit net.")
				if !dr.aggressive {
					log.Fatal("Transit net was ignored, and we are not in aggressive mode. Dying.")
				}
			} else {
				dr.transitNetID = network.ID
				tnet = true
				//if transit net has a gateway, make it drouter's default route
				if len(network.Options["gateway"]) > 0 && !dr.localGateway {
					log.Debugf("Gateway option detected on transit net. Saving for default route.")
					dr.defaultRoute = net.ParseIP(network.Options["gateway"])
				}
			}
		}

		//are we currently connected to this network?
		connected := false
		if n, ok := dr.networks[network.ID]; ok {
			connected = n.connected
		} else {
			dr.networks[network.ID] = &drNetwork{
				drouter: drouter,
				connected: false,
			}
		}

		//is this a container connected network?
		_, cnet := contNetworks[network.ID]

		if drouter {
			if (cnet || tnet || dr.aggressive) && !connected {
				err := dr.drNetworkConnect(&network)
				if err != nil {
					log.Errorf("Error joining network: %v", network)
					continue
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

func isDRouterNetwork(n *dockertypes.NetworkResource) (bool, error) {
	//parse docker network drouter option
	drouter_str := n.Options["drouter"]
	if drouter_str != "" {
		drouter, err := strconv.ParseBool(drouter_str) 
		if err != nil {
			log.Errorf("Error parsing drouter option %v, for network: %v", drouter_str, n.ID)
			return false, err
		}
		return drouter, nil
	}
	return false, nil
}
