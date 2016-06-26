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
	log           *log.Entry
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
	host_link, err := hns.LinkByName("drouter_veth0")
	if err == nil {
		//exists already... delete
		hns.LinkDel(host_link)
	}
	err = nil

	host_link_veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: "drouter_veth0"},
		PeerName:  "drouter_veth1",
	}
	err = hns.LinkAdd(host_link_veth)
	if err != nil {
		logError("Failed to add p2p link", err)
		return nil, err
	}
	host_link, err = hns.LinkByName("drouter_veth0")
	if err != nil {
		log.WithFields(log.Fields{
			"LinkName": "drouter_veth0",
			"Error":    err,
		}).Error("Failed to get p2p link")
		return nil, err
	}

	int_link, err := hns.LinkByName("drouter_veth1")
	if err != nil {
		log.WithFields(log.Fields{
			"LinkName": "drouter_veth1",
			"Error":    err,
		}).Error("Failed to get p2p link")
		return nil, err
	}
	err = hns.LinkSetNsPid(int_link, os.Getpid())
	if err != nil {
		log.WithFields(log.Fields{
			"Pid":   os.Getpid(),
			"Link":  int_link,
			"Error": err,
		}).Error("Failed to put p2p link in self namespace")
		return nil, err
	}
	int_link, err = netlink.LinkByName("drouter_veth1")
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

	host_addr := *p2pIPNet
	host_addr.IP = iputil.IPAdd(host_addr.IP, 1)
	host_netlink_addr := &netlink.Addr{
		IPNet: &host_addr,
		Label: "",
	}
	err = hns.AddrAdd(host_link, host_netlink_addr)
	if err != nil {
		log.WithFields(log.Fields{
			"IP":    host_netlink_addr,
			"Link":  host_link,
			"Error": err,
		}).Error("Failed to add IP to link.")
		return nil, err
	}

	int_addr := *p2pIPNet
	int_addr.IP = iputil.IPAdd(int_addr.IP, 2)
	int_netlink_addr := &netlink.Addr{
		IPNet: &int_addr,
		Label: "",
	}
	err = netlink.AddrAdd(int_link, int_netlink_addr)
	if err != nil {
		log.WithFields(log.Fields{
			"IP":    int_netlink_addr,
			"Link":  int_link,
			"Error": err,
		}).Error("Failed to add IP to link.")
	}

	err = netlink.LinkSetUp(int_link)
	if err != nil {
		log.WithFields(log.Fields{
			"Link":  int_link,
			"Error": err,
		}).Error("Failed to set link up.")
		return nil, err
	}

	err = hns.LinkSetUp(host_link)
	if err != nil {
		log.WithFields(log.Fields{
			"Link":  host_link,
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
		LinkIndex: int_link.Attrs().Index,
		Dst:       iputil.NetworkID(hunderlay),
		Gw:        host_addr.IP,
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
		hostIP:        host_addr.IP,
		selfIP:        int_addr.IP,
		hostNamespace: hns,
		hostUnderlay:  hunderlay,
		log: log.WithFields(log.Fields{
			"p2p": map[string]interface{}{
				"network": p2pIPNet,
				"hostIP":  host_addr.IP,
				"selfIP":  int_addr.IP,
			}}),
	}

	var hostRouteWG sync.WaitGroup
	for _, sr := range staticRoutes {
		if iputil.SubnetContainsSubnet(hroute.Dst, sr) {
			p2pnet.log.WithFields(log.Fields{
				"Subnet": sr,
			}).Debug("Skipping static route to subnet covered by underlay")
			continue
		}
		p2pnet.log.WithFields(log.Fields{
			"Subnet": sr,
		}).Debug("Asynchronously adding host route to subnet.")
		hostRouteWG.Add(1)
		go func() {
			defer hostRouteWG.Done()
			p2pnet.addHostRoute(sr)
		}()
	}

	p2pnet.log.Debug("Waiting for host route adds to complete.")
	hostRouteWG.Wait()
	return p2pnet, nil
}

func (p *p2pNetwork) remove() error {
	p.log.Debug("Removing p2p network")
	host_link, err := p.hostNamespace.LinkByName("drouter_veth0")
	if err != nil {
		p.log.WithFields(log.Fields{
			"LinkName": "drouter_veth0",
			"Error":    err,
		}).Error("Failed to get p2p link")
		return err
	}

	return p.hostNamespace.LinkDel(host_link)
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
	if err != nil {
		log.Error(err)
		p.log.WithFields(log.Fields{
			"Destination": route.Dst,
			"Gateway":     route.Gw,
			"Error":       err,
		}).Error("Failed to add host route.")
	}
	p.log.WithFields(log.Fields{
		"Destination": route.Dst,
		"Gateway":     route.Gw,
	}).Error("Successfully added host route.")
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
	}
	p.log.WithFields(log.Fields{
		"Destination": route.Dst,
		"Gateway":     route.Gw,
	}).Error("Successfully deleted host route.")
}
