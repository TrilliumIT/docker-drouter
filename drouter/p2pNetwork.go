package drouter

import (
	"fmt"
	"net"
	"os"
	"strings"
	"sync"

	log "github.com/Sirupsen/logrus"
	"github.com/TrilliumIT/iputil"
	"github.com/vishvananda/netlink"
)

type p2pNetwork struct {
	network       *net.IPNet
	hostIP        net.IP
	selfIP        net.IP
	hostLinkName  string
	selfLinkName  string
	hostNamespace *netlink.Handle
	hostUnderlay  *net.IPNet
	log           *log.Entry
}

func newP2PNetwork(instance, p2paddr string) (*p2pNetwork, error) {
	log.WithFields(log.Fields{
		"instance": instance,
		"p2paddr":  p2paddr,
	}).Debug("Making p2p network")

	hns, err := netlinkHandleFromPid(1)
	if err != nil {
		logError("Failed to get the host's namespace.", err)
		return nil, err
	}

	_, p2pIPNet, err := net.ParseCIDR(p2paddr)
	if err != nil {
		log.WithFields(log.Fields{
			"Subnet": p2paddr,
			"Error":  err,
		}).Error("Failed to Parse p2p subnet.")
		return nil, err
	}

	hostIP := iputil.IPAdd(p2pIPNet.IP, 1)
	intIP := iputil.IPAdd(p2pIPNet.IP, 2)

	p2pnet := &p2pNetwork{
		network:       p2pIPNet,
		hostIP:        hostIP,
		selfIP:        intIP,
		hostLinkName:  fmt.Sprintf("%v_host", instance),
		selfLinkName:  fmt.Sprintf("%v_self", instance),
		hostNamespace: hns,
		log: log.WithFields(log.Fields{
			"p2p": map[string]interface{}{
				"network": p2pIPNet,
				"hostIP":  hostIP,
				"selfIP":  intIP,
			}}),
	}

	//detect host underlay network
	p2pnet.hostUnderlay, err = p2pnet.getHostUnderlay()
	if err != nil {
		p2pnet.log.Error("Failed to detect host underlay network.")
		return p2pnet, err
	}

	//append the underlay to the static routes slice
	staticRoutes = append(staticRoutes, iputil.NetworkID(p2pnet.hostUnderlay))

	err = p2pnet.createHostLink()
	if err != nil {
		p2pnet.log.Error("Failed to create veth device.")
		return p2pnet, err
	}

	err = p2pnet.addStaticRoutesToHost()
	if err != nil {
		p2pnet.log.Error("Failed to add static routes to host.")
		return p2pnet, err
	}

	return p2pnet, nil
}

func (p *p2pNetwork) remove() error {
	p.log.Debug("Removing p2p network")
	hostLink, err := p.hostNamespace.LinkByName(p.hostLinkName)
	if err != nil {
		p.log.WithFields(log.Fields{
			"LinkName": p.hostLinkName,
			"Error":    err,
		}).Error("Failed to get p2p link")
		return err
	}

	return p.hostNamespace.LinkDel(hostLink)
}

func (p *p2pNetwork) addHostRoute(sn *net.IPNet) {
	p.log.WithFields(log.Fields{
		"Subnet": sn,
	}).Debug("Adding host route to subnet.")
	route := &netlink.Route{
		Gw:  p.selfIP,
		Dst: sn,
		Src: p.hostUnderlay.IP,
	}
	if (route.Dst.IP.To4() == nil) != (route.Gw.To4() == nil) {
		p.log.WithFields(log.Fields{
			"Destination": route.Dst,
			"Gateway":     route.Gw,
		}).Debug("Destination and gateway are different IP family.")
		// Dst is a different IP family
		return
	}

	err := p.hostNamespace.RouteAdd(route)
	if err != nil && !strings.Contains(err.Error(), "file exists") {
		log.Error(err)
		p.log.WithFields(log.Fields{
			"Destination": route.Dst,
			"Gateway":     route.Gw,
			"Error":       err,
		}).Error("Failed to add host route.")
		return
	}
	p.log.WithFields(log.Fields{
		"Destination": route.Dst,
		"Gateway":     route.Gw,
	}).Debug("Successfully added host route.")
}

func (p *p2pNetwork) delHostRoute(sn *net.IPNet) {
	p.log.WithFields(log.Fields{
		"Subnet": sn,
	}).Debug("Deleting host route to subnet.")
	route := &netlink.Route{
		Gw:  p.selfIP,
		Dst: sn,
	}
	if (route.Dst.IP.To4() == nil) != (route.Gw.To4() == nil) {
		// Dst is a different IP family
		p.log.WithFields(log.Fields{
			"Destination": route.Dst,
			"Gateway":     route.Gw,
		}).Debug("Destination and gateway are different IP family.")
		return
	}

	err := p.hostNamespace.RouteDel(route)
	if err != nil {
		p.log.WithFields(log.Fields{
			"Destination": route.Dst,
			"Gateway":     route.Gw,
			"Error":       err,
		}).Error("Failed to delete host route.")
		return
	}
	p.log.WithFields(log.Fields{
		"Destination": route.Dst,
		"Gateway":     route.Gw,
	}).Debug("Successfully deleted host route.")
}

func (p *p2pNetwork) getHostUnderlay() (*net.IPNet, error) {
	hunderlay := &net.IPNet{}
	hroutes, err := p.hostNamespace.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		logError("Failed to get host routes", err)
		return nil, err
	}
Hroutes:
	for _, r := range hroutes {
		if r.Gw != nil {
			continue
		}
		var link netlink.Link
		link, err = p.hostNamespace.LinkByIndex(r.LinkIndex)
		if err != nil {
			log.WithFields(log.Fields{
				"LinkIndex": r.LinkIndex,
				"Error":     err,
			}).Error("Failed to get link.")
			return nil, err
		}
		var addrs []netlink.Addr
		addrs, err = p.hostNamespace.AddrList(link, netlink.FAMILY_V4)
		if err != nil {
			log.WithFields(log.Fields{
				"Link":  link,
				"Error": err,
			}).Error("Failed to get addresses on link.")
			return nil, err
		}
		for _, addr := range addrs {
			if !addr.IP.Equal(r.Src) {
				continue
			}
			hunderlay.IP = addr.IP
			hunderlay.Mask = addr.Mask
			break Hroutes
		}
	}

	log.WithFields(log.Fields{
		"Underlay": hunderlay,
	}).Debug("Discovered host underlay")

	return hunderlay, nil
}

func (p *p2pNetwork) createHostLink() error {
	//check if veth device already exists
	hostLink, err := p.hostNamespace.LinkByName(p.hostLinkName)
	if err == nil {
		//exists already... delete
		err = p.hostNamespace.LinkDel(hostLink)
		if err != nil {
			log.WithFields(log.Fields{"Error": err}).Error("Failed to delete existing p2p interface")
			return err
		}
	}
	err = nil

	//create the link object
	hostLinkVeth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: p.hostLinkName},
		PeerName:  p.selfLinkName,
	}

	//add the link to the host namespace
	err = p.hostNamespace.LinkAdd(hostLinkVeth)
	if err != nil {
		logError("Failed to add p2p link", err)
		return err
	}

	hostLink, err = p.hostNamespace.LinkByName(p.hostLinkName)
	if err != nil {
		log.WithFields(log.Fields{
			"LinkName": p.hostLinkName,
			"Error":    err,
		}).Error("Failed to get host p2p link")
		return err
	}

	//get the dr end of the veth
	intLink, err := p.hostNamespace.LinkByName(p.selfLinkName)
	if err != nil {
		log.WithFields(log.Fields{
			"LinkName": p.selfLinkName,
			"Error":    err,
		}).Error("Failed to get dr p2p link")
		return err
	}

	//move the dr end into the dr namespace
	err = p.hostNamespace.LinkSetNsPid(intLink, os.Getpid())
	if err != nil {
		log.WithFields(log.Fields{
			"Pid":   os.Getpid(),
			"Link":  intLink,
			"Error": err,
		}).Error("Failed to put p2p link in self namespace")
		return err
	}

	//get the new dr end of the veth
	intLink, err = netlink.LinkByName(p.selfLinkName)
	if err != nil {
		log.WithFields(log.Fields{
			"LinkName": p.selfLinkName,
			"Error":    err,
		}).Error("Failed to get p2p link")
		return err
	}

	hostAddr := &net.IPNet{IP: p.hostIP, Mask: p.network.Mask}
	hostNetlinkAddr := &netlink.Addr{
		IPNet: hostAddr,
		Label: "",
	}
	err = p.hostNamespace.AddrAdd(hostLink, hostNetlinkAddr)
	if err != nil {
		log.WithFields(log.Fields{
			"IP":    hostNetlinkAddr,
			"Link":  hostLink,
			"Error": err,
		}).Error("Failed to add IP to host link.")
		return err
	}

	intAddr := &net.IPNet{IP: p.selfIP, Mask: p.network.Mask}
	intNetlinkAddr := &netlink.Addr{
		IPNet: intAddr,
		Label: "",
	}
	err = netlink.AddrAdd(intLink, intNetlinkAddr)
	if err != nil {
		log.WithFields(log.Fields{
			"IP":    intNetlinkAddr,
			"Link":  intLink,
			"Error": err,
		}).Error("Failed to add IP to dr link.")
	}

	err = netlink.LinkSetUp(intLink)
	if err != nil {
		log.WithFields(log.Fields{
			"Link":  intLink,
			"Error": err,
		}).Error("Failed to set dr link up.")
		return err
	}

	err = p.hostNamespace.LinkSetUp(hostLink)
	if err != nil {
		log.WithFields(log.Fields{
			"Link":  hostLink,
			"Error": err,
		}).Error("Failed to set host link up.")
		return err
	}

	return nil
}

func (p *p2pNetwork) addUnderlayRoute() error {
	intLink, err := netlink.LinkByName(p.selfLinkName)
	if err != nil {
		log.WithFields(log.Fields{
			"LinkName": p.selfLinkName,
			"Error":    err,
		}).Error("Failed to get p2p link")
		return err
	}

	hroute := &netlink.Route{
		LinkIndex: intLink.Attrs().Index,
		Dst:       iputil.NetworkID(p.hostUnderlay),
		Gw:        p.hostIP,
	}

	log.WithFields(log.Fields{
		"Destination": p.hostUnderlay,
		"Gateway":     hroute.Gw,
	}).Debug("Adding underlay route to drouter")
	err = netlink.RouteAdd(hroute)
	if err != nil {
		log.WithFields(log.Fields{
			"Destination": p.hostUnderlay,
			"Gateway":     hroute.Gw,
			"Error":       err,
		}).Error("Failed to add underlay route.")
		return err
	}
	return nil
}

func (p *p2pNetwork) addStaticRoutesToHost() error {
	var hostRouteWG sync.WaitGroup
	for _, r := range staticRoutes {
		sr := r
		hostRouteWG.Add(1)
		go func() {
			defer hostRouteWG.Done()
			if iputil.SubnetContainsSubnet(iputil.NetworkID(p.hostUnderlay), sr) {
				p.log.WithFields(log.Fields{
					"Subnet": sr,
				}).Debug("Skipping static route to subnet covered by underlay")
				return
			}
			p.log.WithFields(log.Fields{
				"Subnet": sr,
			}).Debug("Asynchronously adding host route to subnet.")
			p.addHostRoute(sr)
		}()
	}

	p.log.Debug("Waiting for host route adds to complete.")
	hostRouteWG.Wait()
	return nil
}
