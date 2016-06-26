package drouter

import (
	"net"
	"strconv"
	"strings"

	log "github.com/Sirupsen/logrus"
	"github.com/TrilliumIT/iputil"
	dockertypes "github.com/docker/engine-api/types"
	dockernetworks "github.com/docker/engine-api/types/network"
	"github.com/llimllib/ipaddress"
	"github.com/vishvananda/netlink"
	"golang.org/x/net/context"
)

type network struct {
	Name      string
	ID        string
	IPAM      dockernetworks.IPAM
	Options   map[string]string
	log       *log.Entry
	adminDown bool
}

func newNetwork(n *dockertypes.NetworkResource) *network {
	//create the network
	drn := &network{
		ID:        n.ID,
		IPAM:      n.IPAM,
		Options:   n.Options,
		adminDown: false,
		log: log.WithFields(log.Fields{
			"network": map[string]string{
				"Name": n.Name,
				"ID":   n.ID,
			}}),
	}

	return drn
}

func (n *network) logError(msg string, err error) {
	c.log.WithFields(log.Fields{"Error": err}).Error(msg)
}

//connects to a drNetwork
func (drn *network) connect() {
	n.log.Debug("Connecting to network")

	endpointSettings := &dockernetworks.EndpointSettings{}
	//select drouter IP for network
	if ipOffset != 0 {
		var ip net.IP
		n.log.WithFields(log.Fields{
			"ip-offset": ipOffset,
		}).Debug("IP-Offset configured")
		for _, ic := range drn.IPAM.Config {
			_, subnet, err := net.ParseCIDR(ic.Subnet)
			if err != nil {
				n.log.WithFields(log.Fiels{
					"Subnet": ic.Subnet,
					"Error":  err,
				}).Error("Failed to parse subnet.")
				continue
			}
			if ipOffset > 0 {
				ip = iputil.IPAdd(subnet.IP, ipOffset)
			} else {
				last := ipaddress.LastAddress(subnet)
				ip = iputil.IPAdd(last, ipOffset)
			}
			n.log.WithFields(log.Fields{
				"IP":     ip,
				"Subnet": subnet,
			}).Debug("Requesting IP on Subnet.")
			if endpointSettings.IPAddress == "" {
				endpointSettings.IPAddress = ip.String()
				endpointSettings.IPAMConfig = &dockernetworks.EndpointIPAMConfig{
					IPv4Address: ip.String(),
				}
			} else {
				endpointSettings.Aliases = append(endpointSettings.Aliases, ip.String())
			}
		}
	}
	//connect to network
	err := dockerClient.NetworkConnect(context.Background(), drn.ID, selfContainerID, endpointSettings)
	if err != nil {
		if strings.Contains(err.Error(), "already exists") {
			n.log.WithFields(log.Fields{
				"Error": err,
			}).Warning("Attempted to connect to a network that drouter is already connected to")
			return
		}

		n.logError("Failed to connect to network.", err)
		return
	}
	n.log.Debug("Connected to network")
}

//disconnects from this drNetwork
func (drn *network) disconnect() {
	n.log.Debug("Disconnecting from network")
	drn.removeRoutes

	//finished route removal, disconnect
	err := dockerClient.NetworkDisconnect(context.Background(), drn.ID, selfContainerID, true)
	if err != nil {
		n.logError("Failed to disconnect from network", err)
		return
	}
	n.log.Debug("Disconnected from network")
}

func (n *network) isConnected() bool {
	n.log.Debug("Determining if connected")
	routes, err := netlink.RouteList(nil, netlink.FAMILY_ALL)
	if err != nil {
		n.logError("Failed to get routes.", err)
		return false
	}

	for _, ic := range n.IPAM.Config {
		for _, r := range routes {
			if r.Gw != nil {
				continue
			}
			_, subnet, err := net.ParseCIDR(ic.Subnet)
			if err != nil {
				n.log.WithFields(log.Fiels{
					"Subnet": ic.Subnet,
					"Error":  err,
				}).Error("Failed to parse subnet.")
				return false
			}
			if iputil.SubnetEqualSubnet(r.Dst, subnet) {
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
	return n.Options["drouter"] == instanceName || n.ID == transitNetID
}

func (drn *network) connectEvent() error {
	drn.adminDown = false
	return nil
}

func (drn *network) disconnectEvent() error {
	n.log.Debug("Network disconnect detected")
	if !aggressive {
		//do nothing on disconnect event if not aggressive mode
		return nil
	}

	drn.adminDown = true
	drn.removeRoutes()

	return nil
}

func (drn *network) removeRoutes() {
	for _, ic := range drn.IPAM.Config {
		_, sn, err := net.ParseCIDR(ic.Subnet)
		if err != nil {
			n.log.WithFields(log.Fiels{
				"Subnet": ic.Subnet,
				"Error":  err,
			}).Error("Failed to parse subnet.")
			continue
		}
		coveredByStatic := subnetCoveredByStatic(sn)

		//remove host shortcut routes
		if hostShortcut && !coveredByStatic {
			n.log.WithFields(log.Fields{
				"Subnet": sn,
			}).Debug("Asynchronously deleting host routes to subnet.")
			routeDelWG.Add(1)
			go func() {
				defer routeDelWG.Done()
				p2p.delHostRoute(sn)
			}()
		}

		n.log.WithFields(log.Fields{
			"Subnet": sn,
		}).Debug("Asynchronously deleting routes to subnet from all containers.")
		routeDelWG.Add(1)
		go func() {
			defer routeDelWG.Done()
			modifyRoute(nil, sn, DEL_ROUTE)
		}()
	}

	n.log.Debug("Waiting for route removals to complete")
	routeDelWG.Wait()
}
