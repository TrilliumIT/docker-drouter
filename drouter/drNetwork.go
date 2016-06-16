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
	name      string
	drouter   bool
	connected bool
	subnets   []*net.IPNet
}

//connects to a drNetwork
func (dr *DistributedRouter) connectNetwork(id string) error {
	log.Debugf("Connecting to network: %v", dr.networks[id].name)

	endpointSettings := &dockernetworks.EndpointSettings{}

	//select drouter IP for network
	if dr.ipOffset != 0 {
		var ip net.IP
		log.Debugf("ip-offset configured to: %v", dr.ipOffset)
		for _, subnet := range dr.networks[id].subnets {
			log.Debugf("Adding subnet %v", subnet)
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
			log.Debugf("Adding IP %v to network %v", ip, dr.networks[id].name)
		}
	}

	//connect to network
	err := dr.dc.NetworkConnect(context.Background(), id, dr.selfContainerID, endpointSettings)
	if err != nil {
		return err
	}

	dr.networks[id].connected = true

	//if localShortcut add routes to host
	if dr.localShortcut {
		for _, sn := range dr.networks[id].subnets {
			log.Debugf("Injecting shortcut route to %v via drouter into host routing table.", sn)
			route := &netlink.Route{
				LinkIndex: dr.p2p.hostLinkIndex,
				Gw: dr.p2p.selfIP,
				Dst: sn,
				Src: dr.hostUnderlay.IP,
			}
			err = dr.hostNamespace.RouteAdd(route)
			if err != nil {
				return err
			}
		}
	}

	//ensure all local containers also connected to this new network get all of our routes installed
	containers, err := dr.dc.ContainerList(context.Background(), dockertypes.ContainerListOptions{})
	if err != nil {
		log.Error("Failed to get container list.")
		return err
	}
	
	dockerNets, err := dr.dc.NetworkList(context.Background(), dockertypes.NetworkListOptions{ Filters: dockerfilters.NewArgs(), })
	if err != nil {
		log.Error("Failed to list networks.")
		return err
	}

	for _, c := range containers {
		if c.HostConfig.NetworkMode == "host" { continue }
		if c.ID == dr.selfContainerID { continue }

		for _, dn := range dockerNets {
			if _, ok := dr.networks[dn.ID]; !ok { continue }
			if !dr.networks[dn.ID].connected { continue }
			if _, ok := dn.Containers[c.ID]; !ok { continue }

			if dr.localGateway {
				if id != dn.ID { continue }

				cjson, err := dr.dc.ContainerInspect(context.Background(), c.ID)
				if err != nil {
					log.Error(err)
					break
				}
				ch, err := netlinkHandleFromPid(cjson.State.Pid)
				if err != nil {
					log.Error(err)
					break
				}

				gateway, err := dr.getContainerPathIP(ch)
				if err != nil {
					log.Error("Failed to get container path IP.")
					log.Error(err)
					break
				}

				err = dr.replaceContainerGateway(ch, gateway)
				if err != nil {
					log.Error("Failed to replace container gateway.")
					log.Error(err)
					break
				}
				break
			} 

			cjson, err := dr.dc.ContainerInspect(context.Background(), c.ID)
			if err != nil {
				log.Error(err)
				continue
			}
			ch, err := netlinkHandleFromPid(cjson.State.Pid)
			if err != nil {
				log.Error(err)
				continue
			}
			err = dr.addAllContainerRoutes(ch)
			if err != nil {
				log.Error(err)
				continue
			}
		}
	}
	return nil
}

//disconnects from this drNetwork
func (dr *DistributedRouter) disconnectNetwork(id string) error {
	log.Debugf("Attempting to remove network: %v", dr.networks[id].name)

	//ensure all local containers routes are fixed before disconnecting
	containers, err := dr.dc.ContainerList(context.Background(), dockertypes.ContainerListOptions{})
	if err != nil {
		log.Error("Failed to get container list.")
		return err
	}
	
	dockerNets, err := dr.dc.NetworkList(context.Background(), dockertypes.NetworkListOptions{ Filters: dockerfilters.NewArgs(), })
	if err != nil {
		log.Error("Failed to list networks.")
		return err
	}

	for _, c := range containers {
		if c.HostConfig.NetworkMode == "host" { continue }
		if c.ID == dr.selfContainerID { continue }

		dockerNets:
		for _, dn := range dockerNets {
			if !dr.networks[dn.ID].connected { continue }
			if _, ok := dn.Containers[c.ID]; !ok { continue }

			if dr.localGateway {
				if id != dn.ID { continue }

				cjson, err := dr.dc.ContainerInspect(context.Background(), c.ID)
				if err != nil {
					log.Error(err)
					break
				}
				ch, err := netlinkHandleFromPid(cjson.State.Pid)
				if err != nil {
					log.Error(err)
					break
				}

				croutes, err := ch.RouteList(nil, netlink.FAMILY_V4)
				if err != nil {
					log.Error(err)
					break
				}
				for _, cr := range croutes {
					if cr.Dst != nil { continue }
					addrs, err := dr.selfNamespace.AddrList(nil, netlink.FAMILY_V4)
					if err != nil {
						log.Error(err)
						break dockerNets
					}
					me := false
					for _, addr := range addrs {
						if addr.IP.Equal(cr.Gw) {
							me = true
							break
						}
					}
					if !me { break dockerNets }
				}

				gateway, _, err := net.ParseCIDR(dn.Options["gateway"])
				if err != nil {
					log.Error(err)
					break
				}

				err = dr.replaceContainerGateway(ch, gateway)
				if err != nil {
					log.Error("Failed to replace container gateway.")
					log.Error(err)
					break
				}
				break
			} 

			cjson, err := dr.dc.ContainerInspect(context.Background(), c.ID)
			if err != nil {
				log.Error(err)
				continue
			}
			ch, err := netlinkHandleFromPid(cjson.State.Pid)
			if err != nil {
				log.Error(err)
				continue
			}
			if id == dn.ID {
				_, supernet, _ := net.ParseCIDR("0.0.0.0/0")
				err := dr.delContainerRoutes(ch, supernet)
				if err != nil {
					log.Error(err)
					continue
				}
			}
			for _, sn := range dr.networks[id].subnets {
				err := dr.delContainerRoutes(ch, sn)
				if err != nil {
					log.Error(err)
					continue
			}
		}
	}
	return nil
}
	err = dr.dc.NetworkDisconnect(context.Background(), id, dr.selfContainerID, true)
	if err != nil {
		return err
	}
	log.Debugf("Disconnected from network: %v", dr.networks[id].name)

	dr.networks[id].connected = false
	return nil
}

//learns networks from docker and manages connections
func (dr *DistributedRouter) syncNetworks() error {
	log.Debug("Syncing networks from docker.")

	//get all networks from docker
	dockerNets, err := dr.dc.NetworkList(context.Background(), dockertypes.NetworkListOptions{ Filters: dockerfilters.NewArgs(), })
	if err != nil {
		log.Error("Error getting network list")
		return err
	}

	//learn the docker networks
	for _, dn := range dockerNets {
		var err error
		//do we know about this network already?
		if _, ok := dr.networks[dn.ID]; !ok {
			//no, create it
			dr.networks[dn.ID], err = newDRNetwork(&dn)
			if err != nil {
				return err
			}
		}

		if dr.networks[dn.ID].connected {
			continue
		}

		//TODO: move this to initialization
		/*
		//is this network specified as the transit net?
		if dn.Name == dr.transitNet {
			log.Infof("Transit net %v found, and detected as ID: %v", dr.transitNet, dn.ID)
			if !dr.networks[dn.ID].drouter {
				log.Warning("Transit net does not have the drouter option set, but we will treat it as one anyway.")
				dr.networks[dn.ID].drouter = true
			}
			dr.transitNetID = dn.ID
			//if transit net has a gateway, make it drouter's default route
			if len(dn.Options["gateway"]) > 0 && !dr.localGateway {
				dr.defaultRoute = net.ParseIP(dn.Options["gateway"])
				log.Debugf("Gateway option detected on transit net as: %v", dr.defaultRoute)
			}
		}
		*/

		if dr.networks[dn.ID].drouter {
			err := dr.connectNetwork(dn.ID)
			if err != nil {
				log.Error(err)
				continue
			}
			//if we manage drouters default route, fix it
			if dr.defaultRoute != nil && !dr.defaultRoute.Equal(net.IP{}) {
				//ensure default route for drouter is correct
				err = dr.setDefaultRoute()
				if err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func newDRNetwork(n *dockertypes.NetworkResource) (*drNetwork, error) {
	log.Debugf("Learning a new network: %v", n.Name)
	var err error

	//parse docker network drouter option
	drouter := false
	drouter_str := n.Options["drouter"]
	if drouter_str != "" {
		drouter, err = strconv.ParseBool(drouter_str) 
		if err != nil {
			log.Errorf("Error parsing drouter option %v, for network: %v", drouter_str, n.ID)
			return &drNetwork{}, err
		}
	}

	//get the network prefixes
	subnets := make([]*net.IPNet, len(n.IPAM.Config))

	for i, ipamconfig := range n.IPAM.Config {
		_, subnets[i], err = net.ParseCIDR(ipamconfig.Subnet)
		if err != nil {
			log.Error(err)
			continue
		}
	}

	//create the network
	drn := &drNetwork{
		name: n.Name,
		drouter: drouter,
		connected: false,
		subnets: subnets,
	}

	return drn, nil
}
