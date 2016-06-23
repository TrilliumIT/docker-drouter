package drouter

import (
	log "github.com/Sirupsen/logrus"
	dockertypes "github.com/docker/engine-api/types"
	dockernetworks "github.com/docker/engine-api/types/network"
	"github.com/llimllib/ipaddress"
	"github.com/vishvananda/netlink"
	"github.com/ziutek/utils/netaddr"
	"golang.org/x/net/context"
	"net"
	"strconv"
	"strings"
)

type network struct {
	Name      string
	ID        string
	IPAM      dockernetworks.IPAM
	Options   map[string]string
	adminDown bool
}

//connects to a drNetwork
func (drn *network) connect() {
	log.Debugf("Connecting to network: %v", drn.Name)

	endpointSettings := &dockernetworks.EndpointSettings{}
	//select drouter IP for network
	if ipOffset != 0 {
		var ip net.IP
		log.Debugf("ip-offset configured to: %v", ipOffset)
		for _, ic := range drn.IPAM.Config {
			_, subnet, err := net.ParseCIDR(ic.Subnet)
			if err != nil {
				log.Error(err)
			}
			log.Debugf("Adding subnet %v", subnet)
			if ipOffset > 0 {
				ip = netaddr.IPAdd(subnet.IP, ipOffset)
			} else {
				last := ipaddress.LastAddress(subnet)
				ip = netaddr.IPAdd(last, ipOffset)
			}
			if endpointSettings.IPAddress == "" {
				endpointSettings.IPAddress = ip.String()
				endpointSettings.IPAMConfig = &dockernetworks.EndpointIPAMConfig{
					IPv4Address: ip.String(),
				}
			} else {
				endpointSettings.Aliases = append(endpointSettings.Aliases, ip.String())
			}
			log.Debugf("Adding IP %v to network %v", ip, drn.Name)
		}
	}
	//connect to network
	err := dockerClient.NetworkConnect(context.Background(), drn.ID, selfContainerID, endpointSettings)
	if err != nil {
		if strings.Contains(err.Error(), "already exists") {
			log.Warningf("Attempted to connect to network that drouter is already connected to: %v", drn.Name)
			return
		}

		log.Errorf("Error connecting to network: %v", drn.Name)
		log.Error(err)
		return
	}
	log.Debugf("Connected to network: %v", drn.Name)
}

//disconnects from this drNetwork
func (drn *network) disconnect() {
	log.Debugf("Disconnecting from network: %v", drn.Name)

	//first, loop through all subnets in this network
	for _, ic := range drn.IPAM.Config {
		_, sn, err := net.ParseCIDR(ic.Subnet)
		if err != nil {
			log.Error(err)
			continue
		}

		coveredByStatic := subnetCoveredByStatic(sn)

		//remove host shortcut routes
		if localShortcut && !coveredByStatic {
			go p2p.delHostRoute(sn)
		}

		modifyRoute(nil, sn, DEL_ROUTE)
	}

	//finished route removal, disconnect
	err := dockerClient.NetworkDisconnect(context.Background(), drn.ID, selfContainerID, true)
	if err != nil {
		log.Error(err)
		return
	}
	log.Debugf("Disconnected from network: %v", drn.Name)
}

func newNetwork(n *dockertypes.NetworkResource) *network {
	//create the network
	drn := &network{
		Name:      n.Name,
		ID:        n.ID,
		IPAM:      n.IPAM,
		Options:   n.Options,
		adminDown: false,
	}

	return drn
}

func (n *network) isConnected() bool {
	routes, err := netlink.RouteList(nil, netlink.FAMILY_ALL)
	if err != nil {
		log.Error("Failed to get self routes.")
		log.Error(err)
		return false
	}

	for _, ic := range n.IPAM.Config {
		for _, r := range routes {
			if r.Gw != nil {
				continue
			}
			_, subnet, err := net.ParseCIDR(ic.Subnet)
			if err != nil {
				log.Error("Failed to parse ipam subnet.")
				log.Error(err)
				return false
			}
			if subnetEqualSubnet(r.Dst, subnet) {
				//if we are connected to /any/ subnet, then we must be connected to the vxlan already
				//if we are missing only one subnet, we can't re-connect anyway
				//so don't continue here
				return true
			}
		}
	}
	return false
}

func (n *network) isDRouter() bool {
	if n.ID == transitNetID {
		return true
	}
	var err error
	drouter := false

	//parse docker network drouter option
	drouter_str := n.Options["drouter"]
	if drouter_str != "" {
		drouter, err = strconv.ParseBool(drouter_str)
		if err != nil {
			log.Errorf("Error parsing drouter option %v, for network: %v", drouter_str, n.ID)
			log.Error(err)
			return false
		}
	}

	return drouter
}

func (drn *network) connectEvent() error {
	drn.adminDown = false

	return nil
}

func (drn *network) disconnectEvent() error {
	if !aggressive {
		//do nothing on disconnect event if not aggressive mode
		return nil
	}

	drn.adminDown = true
	log.Debugf("Detected disconnect from network: %v", drn.Name)

	//first, loop through all subnets in this network
	for _, ic := range drn.IPAM.Config {
		_, sn, err := net.ParseCIDR(ic.Subnet)
		if err != nil {
			log.Error(err)
			continue
		}

		coveredByStatic := subnetCoveredByStatic(sn)

		//remove host shortcut routes
		if localShortcut && !coveredByStatic {
			go p2p.delHostRoute(sn)
		}

		modifyRoute(nil, sn, DEL_ROUTE)
	}

	return nil
}
