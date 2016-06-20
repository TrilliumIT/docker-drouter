package main

import (
	log "github.com/Sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"net"
	"syscall"
)

type routeUpdate struct {
	src net.Addr
	msg []byte
}

func addrToIP(s net.Addr) (net.IP, error) {
	srcS, _, err := net.SplitHostPort(s.String())
	if err != nil {
		return nil, err
	}
	return net.ParseIP(srcS), nil

}

func delAllRoutesVia(s net.Addr) error {
	src, err := addrToIP(s)
	if err != nil {
		log.Error("Error parsing IP from %v", s)
		return err
	}

	routes, err := netlink.RouteList(nil, netlink.FAMILY_ALL)
	if err != nil {
		log.Error("Failed to get routes")
		log.Error(err)
		return err
	}
	for _, r := range routes {
		if r.Gw.Equal(src) {
			netlink.RouteDel(&r)
		}
	}
	return nil
}

type exportRoute struct {
	Type     uint16
	Dst      *net.IPNet
	Gw       net.IP
	Priority int
}

func processRoute(ru *exportRoute, s net.Addr) error {
	src, err := addrToIP(s)
	if err != nil {
		log.Error("Error parsing IP from %v", s)
		return err
	}

	addrs, err := netlink.AddrList(nil, netlink.FAMILY_ALL)
	if err != nil {
		log.Error("Failed to get addresses")
		log.Error(err)
		return err
	}

	for _, addr := range addrs {
		if addr.IP.Equal(ru.Gw) {
			log.Debugf("Route back to self, disregard")
			return nil
		}
	}

	switch {
	case ru.Type == syscall.RTM_NEWROUTE:
		r := &netlink.Route{
			Dst:      ru.Dst,
			Gw:       src,
			Priority: ru.Priority + 100,
		}
		err := netlink.RouteAdd(r)
		if err != nil {
			log.Errorf("Failed to add route: %v", r)
			log.Error(err)
			return err
		}
	case ru.Type == syscall.RTM_DELROUTE:
		routes, err := netlink.RouteGet(ru.Dst.IP)
		if err != nil {
			log.Error("Failed to get routes")
			log.Error(err)
			return err
		}
		for _, r := range routes {
			if r.Gw.Equal(src) {
				netlink.RouteDel(&r)
			}
		}
	}
	return nil
}
