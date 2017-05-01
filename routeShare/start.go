package routeShare

import (
	log "github.com/Sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"net"
	"sync"
	"syscall"
)

type RouteShare struct {
	ip               net.IP
	port             int
	instance         int
	quit             <-chan struct{}
	localRouteUpdate chan *exportRoute
}

func NewRouteShare(ip net.IP, port, instance int, quit <-chan struct{}) *RouteShare {
	return &RouteShare{
		ip:               ip,
		port:             port,
		instance:         instance,
		quit:             quit,
		localRouteUpdate: make(chan *exportRoute),
	}
}

func (r *RouteShare) Start() error {
	var wg sync.WaitGroup

	connectPeer := make(chan string)
	defer close(connectPeer)

	hc := make(chan *hello)

	ech := make(chan error)
	done := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		ech <- r.startHello(connectPeer, hc, done)
	}()

	helloMsg := <-hc
	wg.Add(1)
	go func() {
		defer wg.Done()
		r.startPeer(connectPeer, helloMsg, done)
	}()

	var err error
	select {
	case err = <-ech:
		close(done)
	case <-r.quit:
		close(done)
		err = <-ech
	}

	wg.Wait()
	return err
}

const (
	AddRoute = true
	DelRoute = false
)

func (r *RouteShare) ModifyRoute(dst *net.IPNet, action bool) {
	if !isDirect(dst.IP) {
		log.WithField("dst", dst).Debug("Refusing to publish non-direct route")
		return
	}
	er := &exportRoute{
		Dst: dst,
	}
	if action == AddRoute {
		er.Type = syscall.RTM_NEWROUTE
		log.Debugf("Sending route add: %v", er)
	}
	if action == DelRoute {
		er.Type = syscall.RTM_DELROUTE
		log.Debugf("Sending route delete: %v", er)
	}
	r.localRouteUpdate <- er
}

func (r *RouteShare) AddRoute(dst *net.IPNet) {
	r.ModifyRoute(dst, AddRoute)
}

func (r *RouteShare) DelRoute(dst *net.IPNet) {
	r.ModifyRoute(dst, DelRoute)
}

func (r *RouteShare) ShareRouteUpdate(ru *netlink.RouteUpdate) {
	if ru.Gw != nil {
		// we only care about directly connected routes
		return
	}
	if ru.Table == 255 {
		// We don't want entries from the local routing table
		// http://linux-ip.net/html/routing-tables.html
		return
	}
	if ru.Src.IsLoopback() {
		return
	}
	if ru.Dst.IP.IsLoopback() {
		return
	}
	if ru.Src.IsLinkLocalUnicast() {
		return
	}
	if ru.Dst.IP.IsLinkLocalUnicast() {
		return
	}
	if ru.Dst.IP.IsInterfaceLocalMulticast() {
		return
	}
	if ru.Dst.IP.IsLinkLocalMulticast() {
		return
	}
	er := &exportRoute{
		Type:     ru.Type,
		Dst:      ru.Dst,
		Gw:       ru.Gw,
		Priority: ru.Priority,
	}
	log.Debugf("Sending route update: %v", er)
	r.localRouteUpdate <- er
}
