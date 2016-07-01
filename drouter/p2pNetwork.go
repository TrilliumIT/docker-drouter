package drouter

import (
	"net"
	"os"
	"sync"

	log "github.com/Sirupsen/logrus"
	"github.com/TrilliumIT/iputil"
	"github.com/vishvananda/netlink"
)

type p2pNetwork struct {
	network       *net.IPNet
	hostIP        net.IP
	selfIP        net.IP
	hostNamespace *netlink.Handle
	hostUnderlay  *net.IPNet
	hostLink      *log.Entry
}

func newP2PNetwork(p2paddr string) (*p2pNetwork, error) {
	hns, err := netlinkHandleFromPid(1)
	if err != nil {
		logError("Failed to get the host's namespace.", err)
		return nil, err
	}

	log.WithFields(log.Fields{
		"p2paddr": p2paddr,
	}).Debug("Making p2p network")

	//check if drouter_veth0 already exists
	hostLink, err := hns.LinkByName("drouter_veth0")
	if err == nil {
		//exists already... delete
		hns.LinkDel(hostLink)
	}
	err = nil

	hostLinkVeth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: "drouter_veth0"},
		PeerName:  "drouter_veth1",
	}
	err = hns.LinkAdd(hostLinkVeth)
	if err != nil {
		logError("Failed to add p2p link", err)
		return nil, err
	}
	hostLink, err = hns.LinkByName("drouter_veth0")
	if err != nil {
		log.WithFields(log.Fields{
			"LinkName": "drouter_veth0",
			"Error":    err,
		}).Error("Failed to get p2p link")
		return nil, err
	}

	intLink, err := hns.LinkByName("drouter_veth1")
	if err != nil {
		log.WithFields(log.Fields{
			"LinkName": "drouter_veth1",
			"Error":    err,
		}).Error("Failed to get p2p link")
		return nil, err
	}
	err = hns.LinkSetNsPid(intLink, os.Getpid())
	if err != nil {
		log.WithFields(log.Fields{
			"Pid":   os.Getpid(),
			"Link":  intLink,
			"Error": err,
		}).Error("Failed to put p2p link in self namespace")
		return nil, err
	}
	intLink, err = netlink.LinkByName("drouter_veth1")
	if err != nil {
		log.WithFields(log.Fields{
			"LinkName": "drouter_veth1",
			"Error":    err,
		}).Error("Failed to get p2p link")
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

	hostAddr := *p2pIPNet
	hostAddr.IP = iputil.IPAdd(hostAddr.IP, 1)
	hostNetlinkAddr := &netlink.Addr{
		IPNet: &hostAddr,
		Label: "",
	}
	err = hns.AddrAdd(hostLink, hostNetlinkAddr)
	if err != nil {
		log.WithFields(log.Fields{
			"IP":    hostNetlinkAddr,
			"Link":  hostLink,
			"Error": err,
		}).Error("Failed to add IP to link.")
		return nil, err
	}

	intAddr := *p2pIPNet
	intAddr.IP = iputil.IPAdd(intAddr.IP, 2)
	intNetlinkAddr := &netlink.Addr{
		IPNet: &intAddr,
		Label: "",
	}
	err = netlink.AddrAdd(intLink, intNetlinkAddr)
	if err != nil {
		log.WithFields(log.Fields{
			"IP":    intNetlinkAddr,
			"Link":  intLink,
			"Error": err,
		}).Error("Failed to add IP to link.")
	}

	err = netlink.LinkSetUp(intLink)
	if err != nil {
		log.WithFields(log.Fields{
			"Link":  intLink,
			"Error": err,
		}).Error("Failed to set link up.")
		return nil, err
	}

	err = hns.LinkSetUp(hostLink)
	if err != nil {
		log.WithFields(log.Fields{
			"Link":  hostLink,
			"Error": err,
		}).Error("Failed to set link up.")
		return nil, err
	}

	//discover host underlay address/network
	hunderlay := &net.IPNet{}
	hroutes, err := hns.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		logError("Failed to get host routes", err)
		return nil, err
	}
Hroutes:
	for _, r := range hroutes {
		if r.Gw != nil {
			continue
		}
		link, err := hns.LinkByIndex(r.LinkIndex)
		if err != nil {
			log.WithFields(log.Fields{
				"LinkIndex": r.LinkIndex,
				"Error":     err,
			}).Error("Failed to get link.")
			return nil, err
		}
		addrs, err := hns.AddrList(link, netlink.FAMILY_V4)
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

	staticRoutes = append(staticRoutes, iputil.NetworkID(hunderlay))

	hroute := &netlink.Route{
		LinkIndex: intLink.Attrs().Index,
		Dst:       iputil.NetworkID(hunderlay),
		Gw:        hostAddr.IP,
	}

	log.WithFields(log.Fields{
		"Destination": hunderlay,
		"Gateway":     hroute.Gw,
	}).Debug("Adding underlay route to drouter")
	err = netlink.RouteAdd(hroute)
	if err != nil {
		log.WithFields(log.Fields{
			"Destination": hunderlay,
			"Gateway":     hroute.Gw,
			"Error":       err,
		}).Error("Failed to add underlay route.")
		return nil, err
	}

	p2pnet := &p2pNetwork{
		network:       p2pIPNet,
		hostIP:        hostAddr.IP,
		selfIP:        intAddr.IP,
		hostNamespace: hns,
		hostUnderlay:  hunderlay,
		hostLink: log.WithFields(log.Fields{
			"p2p": map[string]interface{}{
				"network": p2pIPNet,
				"hostIP":  hostAddr.IP,
				"selfIP":  intAddr.IP,
			}}),
	}

	var hostRouteWG sync.WaitGroup
	for _, r := range staticRoutes {
		sr := r
		go func() {
			defer hostRouteWG.Done()
			if iputil.SubnetContainsSubnet(hroute.Dst, sr) {
				p2pnet.hostLink.WithFields(log.Fields{
					"Subnet": sr,
				}).Debug("Skipping static route to subnet covered by underlay")
				return
			}
			p2pnet.hostLink.WithFields(log.Fields{
				"Subnet": sr,
			}).Debug("Asynchronously adding host route to subnet.")
			hostRouteWG.Add(1)
			p2pnet.addHostRoute(sr)
		}()
	}

	p2pnet.hostLink.Debug("Waiting for host route adds to complete.")
	hostRouteWG.Wait()
	return p2pnet, nil
}

func (p *p2pNetwork) remove() error {
	p.hostLink.Debug("Removing p2p network")
	hostLink, err := p.hostNamespace.LinkByName("drouter_veth0")
	if err != nil {
		p.hostLink.WithFields(log.Fields{
			"LinkName": "drouter_veth0",
			"Error":    err,
		}).Error("Failed to get p2p link")
		return err
	}

	return p.hostNamespace.LinkDel(hostLink)
}

func (p *p2pNetwork) addHostRoute(sn *net.IPNet) {
	p.hostLink.WithFields(log.Fields{
		"Subnet": sn,
	}).Debug("Adding host route to subnet.")
	route := &netlink.Route{
		Gw:  p.selfIP,
		Dst: sn,
		Src: p.hostUnderlay.IP,
	}
	if (route.Dst.IP.To4() == nil) != (route.Gw.To4() == nil) {
		p.hostLink.WithFields(log.Fields{
			"Destination": route.Dst,
			"Gateway":     route.Gw,
		}).Debug("Destination and gateway are different IP family.")
		// Dst is a different IP family
		return
	}

	err := p.hostNamespace.RouteAdd(route)
	if err != nil {
		log.Error(err)
		p.hostLink.WithFields(log.Fields{
			"Destination": route.Dst,
			"Gateway":     route.Gw,
			"Error":       err,
		}).Error("Failed to add host route.")
	}
	p.hostLink.WithFields(log.Fields{
		"Destination": route.Dst,
		"Gateway":     route.Gw,
	}).Error("Successfully added host route.")
}

func (p *p2pNetwork) delHostRoute(sn *net.IPNet) {
	p.hostLink.WithFields(log.Fields{
		"Subnet": sn,
	}).Debug("Deleting host route to subnet.")
	route := &netlink.Route{
		Gw:  p.selfIP,
		Dst: sn,
	}
	if (route.Dst.IP.To4() == nil) != (route.Gw.To4() == nil) {
		// Dst is a different IP family
		p.hostLink.WithFields(log.Fields{
			"Destination": route.Dst,
			"Gateway":     route.Gw,
		}).Debug("Destination and gateway are different IP family.")
		return
	}

	err := p.hostNamespace.RouteDel(route)
	if err != nil {
		p.hostLink.WithFields(log.Fields{
			"Destination": route.Dst,
			"Gateway":     route.Gw,
			"Error":       err,
		}).Error("Failed to delete host route.")
	}
	p.hostLink.WithFields(log.Fields{
		"Destination": route.Dst,
		"Gateway":     route.Gw,
	}).Error("Successfully deleted host route.")
}
