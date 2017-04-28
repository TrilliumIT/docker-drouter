package drouter

import (
	"net"
	"strings"
	"sync"

	log "github.com/Sirupsen/logrus"
	"github.com/TrilliumIT/iputil"
	dockertypes "github.com/docker/engine-api/types"
	dockernetworks "github.com/docker/engine-api/types/network"
	"github.com/llimllib/ipaddress"
	"github.com/vishvananda/netlink"
	"golang.org/x/net/context"
)

type network struct {
	Name        string
	ID          string
	Subnets     []*net.IPNet
	Options     map[string]string
	log         *log.Entry
	connectLock sync.RWMutex
	adminDown   bool
}

func newNetwork(n *dockertypes.NetworkResource) *network {
	//create the network
	drn := &network{
		Name:      n.Name,
		ID:        n.ID,
		Options:   n.Options,
		adminDown: false,
		log: log.WithFields(log.Fields{
			"network": map[string]string{
				"Name": n.Name,
				"ID":   n.ID,
			}}),
	}
	for _, ic := range n.IPAM.Config {
		_, subnet, err := net.ParseCIDR(ic.Subnet)
		if err != nil {
			log.WithFields(log.Fields{
				"Subnet": ic.Subnet,
				"Error":  err,
			}).Error("Failed to parse IPAM Subnet.")
			continue
		}
		drn.Subnets = append(drn.Subnets, subnet)
	}

	return drn
}

func (n *network) logError(msg string, err error) {
	n.log.WithFields(log.Fields{"Error": err}).Error(msg)
}

//connects to a drNetwork
func (n *network) connect() {
	n.log.Debug("Connecting to network")
	n.connectLock.Lock()
	defer n.connectLock.Unlock()
	if isConnected(n) {
		n.log.Debug("Already connected.")
		return
	}

	endpointSettings := &dockernetworks.EndpointSettings{}
	//select drouter IP for network
	if ipOffset != 0 {
		var ip net.IP
		n.log.WithFields(log.Fields{
			"ip-offset": ipOffset,
		}).Debug("IP-Offset configured")
		for _, subnet := range n.Subnets {
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
	err := dockerClient.NetworkConnect(context.Background(), n.ID, selfContainerID, endpointSettings)
	if err != nil {
		// this should never happen, we already checked for it.
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
func (n *network) disconnect() {
	n.connectLock.Lock()
	defer n.connectLock.Unlock()
	if !isConnected(n) {
		return
	}

	n.log.Debug("Disconnecting from network")
	n.removeRoutes()

	//finished route removal, disconnect
	err := dockerClient.NetworkDisconnect(context.Background(), n.ID, selfContainerID, true)
	if err != nil {
		n.logError("Failed to disconnect from network", err)
		return
	}
	n.log.Debug("Disconnected from network")
}

func (n *network) isConnected() bool {
	n.connectLock.RLock()
	defer n.connectLock.RUnlock()
	return isConnected(n)
}

func isConnected(n *network) bool {
	routes, err := netlink.RouteList(nil, netlink.FAMILY_ALL)
	if err != nil {
		n.logError("Failed to get routes.", err)
		return false
	}

	for _, subnet := range n.Subnets {
		for _, r := range routes {
			if r.Gw != nil {
				continue
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

func (n *network) connectEvent() error {
	n.adminDown = false
	return nil
}

func (n *network) disconnectEvent() error {
	n.log.Debug("Network disconnect detected")
	if !aggressive {
		//do nothing on disconnect event if not aggressive mode
		return nil
	}

	n.adminDown = true
	n.removeRoutes()

	return nil
}

func (n *network) removeRoutes() {
	var routeDelWG sync.WaitGroup
	for _, subnet := range n.Subnets {
		sn := subnet
		routeDelWG.Add(1)
		go func() {
			defer routeDelWG.Done()
			coveredByStatic := subnetCoveredByStatic(sn)

			//remove host shortcut routes
			if hostShortcut && !coveredByStatic {
				routeDelWG.Add(1)
				go func() {
					defer routeDelWG.Done()
					n.log.WithFields(log.Fields{
						"Subnet": sn,
					}).Debug("Deleting host routes to subnet.")
					p2p.delHostRoute(sn)
				}()
			}

			n.log.WithFields(log.Fields{
				"Subnet": sn,
			}).Debug("Deleting routes to subnet from all containers.")
			err := modifyRoute(sn, delRoute)
			if err != nil {
				n.logError("Failed to delete routes", err)
			}
		}()
	}

	n.log.Debug("Waiting for route removals to complete")
	routeDelWG.Wait()
}
