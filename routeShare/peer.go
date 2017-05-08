package routeShare

import (
	"encoding/gob"
	"net"
	"strings"
	"sync"
	"syscall"

	log "github.com/Sirupsen/logrus"
	"github.com/vishvananda/netlink"
)

type subscriber struct {
	cb  chan *exportRoute
	del chan struct{}
}

func startTCPListener(peerCh chan<- *net.Conn, lAddr string, done <-chan struct{}) {
	listener, err := net.Listen("tcp", lAddr)
	if err != nil {
		log.Error("Failed to setup listener")
		log.Error(err)
		return
	}
	go func() {
		<-done
		err := listener.Close()
		if err != nil {
			log.WithError(err).Error("Error closing tcp listener")
		}
	}()

	for {
		c, err := listener.Accept()
		if err != nil {
			if strings.HasSuffix(err.Error(), "use of closed network connection") {
				return
			}
			log.WithError(err).Error("Failed to accept listener")
			continue
		}
		if !isDirect(c.RemoteAddr().(*net.TCPAddr).IP) {
			log.Errorf("Rejecting connection to not directly connected peer")
			continue
		}
		peerCh <- &c
	}
}

func isDirect(src net.IP) bool {
	routes, err := netlink.RouteGet(src)
	if err != nil {
		log.Errorf("Failed to get route to src %v", src)
		return false
	}
	for _, route := range routes {
		if route.Gw == nil {
			return true
		}
	}
	return false
}

type procConReqOpts struct {
	peerCh         chan<- *net.Conn
	connectPeer    <-chan string
	peerConnected  <-chan string
	disconnectPeer <-chan string
}

func processConnectionRequest(o *procConReqOpts, done <-chan struct{}) {
	peers := make(map[string]struct{})
	for {
		select {
		case <-done:
			return
		case p := <-o.connectPeer:
			if _, ok := peers[p]; !ok {
				c, err := net.Dial("tcp", p)
				if err != nil {
					log.Error("Failed to dial")
					log.Error(err)
					continue
				}
				o.peerCh <- &c
			}
		case p := <-o.peerConnected:
			peers[p] = struct{}{}
			log.Debugf("Current peers: %v", peers)
		case p := <-o.disconnectPeer:
			delete(peers, p)
			log.Debugf("Deleted peer %v", p)
			log.Debugf("Current peers: %v", peers)
		}
	}
}

func (r *RouteShare) startPeer(connectPeer <-chan string, helloMsg *hello, done <-chan struct{}) {
	wg := sync.WaitGroup{}

	// listen for incoming tcp connections
	peerCh := make(chan *net.Conn)
	wg.Add(1)
	go func() {
		startTCPListener(peerCh, helloMsg.ListenAddr, done)
		log.Debug("tcplistener done")
		wg.Done()
	}()

	// process new connections, from hello or tcp
	peerConnected := make(chan string)
	disconnectPeer := make(chan string)
	procOpts := &procConReqOpts{
		peerCh:         peerCh,
		connectPeer:    connectPeer,
		peerConnected:  peerConnected,
		disconnectPeer: disconnectPeer,
	}
	crWg := sync.WaitGroup{}
	crDone := make(chan struct{})
	crWg.Add(1)
	go func() {
		processConnectionRequest(procOpts, crDone)
		log.Debug("processconnectionrequests done")
		crWg.Done()
	}()

	// Handle new peers joining
	var subscribers []*subscriber
	addSubscriber := make(chan *subscriber)

	peerOpts := &peerConnOpts{
		helloMsg:       helloMsg,
		peerCh:         peerCh,
		peerConnected:  peerConnected,
		addSubscriber:  addSubscriber,
		disconnectPeer: disconnectPeer,
	}
	wg.Add(1)
	go func() {
		peerConnections(peerOpts, r.priority, done)
		log.Debug("peerconnections done")
		wg.Done()
	}()

	// Manage listening peers
	delSubscriber := make(chan int)
	localRouteUpdates := make(map[string]*exportRoute)
	for {
		select {
		case <-done:
			wg.Wait()
			close(crDone)
			crWg.Wait()
			return
		case d := <-delSubscriber:
			if len(subscribers) <= 1 {
				subscribers = []*subscriber{}
				continue
			}
			subscribers = append(subscribers[:d], subscribers[d+1:]...)
		case s := <-addSubscriber:
			i := len(subscribers)
			subscribers = append(subscribers, s)
			// Start a routine to listen for closing s.del
			// send delSubscriber for the position of s when triggered
			for _, r := range localRouteUpdates {
				go func(r *exportRoute) {
					select {
					// just in case a del is pending
					case <-s.del:
						break
					default:
						s.cb <- r
					}
				}(r)
			}
			go func() {
				<-s.del
				close(s.cb)
				delSubscriber <- i
			}()
		case r := <-r.localRouteUpdate:
			switch r.Type {
			case syscall.RTM_NEWROUTE:
				localRouteUpdates[r.Dst.String()] = r
			case syscall.RTM_DELROUTE:
				delete(localRouteUpdates, r.Dst.String())
			}

			for _, s := range subscribers {
				select {
				// just in case a del is pending
				case <-s.del:
					break
				default:
					s.cb <- r
				}
			}
		}
	}
}

type peerConnOpts struct {
	helloMsg       *hello
	peerCh         <-chan *net.Conn
	peerConnected  chan<- string
	addSubscriber  chan<- *subscriber
	disconnectPeer chan<- string
}

func peerConnections(o *peerConnOpts, priority int, done <-chan struct{}) {
	wg := sync.WaitGroup{}
	for {
		var c net.Conn
		select {
		case cp := <-o.peerCh:
			c = *cp
		case <-done:
			wg.Wait()
			return
		}

		// Send hello on all new tcp connections
		e := gob.NewEncoder(c)
		err := e.Encode(o.helloMsg)
		if err != nil {
			log.Error("Failed to encode tcp hello on connect")
			log.Error(err)
			return
		}

		// get corresponding hello from the other side
		d := gob.NewDecoder(c)
		h := &hello{}
		err = d.Decode(h)
		if err != nil {
			log.Error("Failed to decode tcp hello on connect")
			log.Error(err)
			return
		}

		log.Debugf("hello recieved via tcp: %v", h)
		o.peerConnected <- h.ListenAddr

		// Handle messages with this peer
		s := &subscriber{
			cb:  make(chan *exportRoute),
			del: make(chan struct{}),
		}
		o.addSubscriber <- s

		// die gracefully
		sendDie := make(chan struct{})
		recvDie := make(chan struct{})
		wg.Add(1)
		go func() {
			defer log.Debug("sendDie done")
			defer wg.Done()
			select {
			case <-sendDie:
			case <-recvDie:
			case <-done:
			}
			close(s.del)
			o.disconnectPeer <- h.ListenAddr
			err := delAllRoutesVia(c.RemoteAddr())
			if err != nil {
				log.Errorf("Failed to delete all routes via %v", c.RemoteAddr())
			}
			err = c.Close()
			if err != nil {
				log.WithError(err).Error("Error closing peer connection")
			}
		}()

		// Send updates to this peer
		wg.Add(1)
		go func() {
			defer log.Debug("encode done")
			defer wg.Done()
			e := gob.NewEncoder(c)
			srcAddr := c.LocalAddr().(*net.TCPAddr).IP
			for r := range s.cb {
				if r == nil { // r is closed on subscriber
					return
				}
				if (r.Dst.IP.To4() == nil) != (srcAddr.To4() == nil) {
					// Families don't match
					continue
				}
				err := e.Encode(r)
				if err != nil {
					log.Error("Failed to encode exportRoute")
					log.Error(err)
					close(sendDie)
					return
				}
			}
		}()

		// Recieve messages from this peer
		wg.Add(1)
		go func() {
			defer log.Debug("decode done")
			defer wg.Done()
			d := gob.NewDecoder(c)
			for {
				er := &exportRoute{}
				err := d.Decode(er)
				if err != nil {
					if strings.HasSuffix(err.Error(), "use of closed network connection") {
						return
					}
					log.WithError(err).Error("Failed to decode route update")
					close(recvDie)
					return
				}

				log.Debugf("Recieved update %v from %v", *er, c.RemoteAddr())
				err = processRoute(er, c.RemoteAddr(), priority)
				if err != nil {
					log.Errorf("Failed to process route: %v", *er)
					continue
				}
			}
		}()
	}
}
