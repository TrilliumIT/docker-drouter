package main

import (
	"encoding/gob"
	log "github.com/Sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"net"
	"strconv"
)

type subscriber struct {
	cb  chan *exportRoute
	del chan struct{}
}

func startPeer(connectPeer <-chan string, hc <-chan hello) {
	helloMsg := <-hc

	localRouteUpdate := make(chan *exportRoute)

	// Monitor local route table changes
	go func() {
		rud := make(chan struct{})
		defer close(rud)
		ruc := make(chan netlink.RouteUpdate)
		defer close(ruc)
		err := netlink.RouteSubscribe(ruc, rud)
		if err != nil {
			log.Error("Error subscribing to route table")
			log.Fatal(err)
		}
		for {
			ru := <-ruc
			if ru.Gw != nil {
				// we only care about directly connected routes
				continue
			}
			if ru.Src.IsLoopback() {
				continue
			}
			if ru.Dst.IP.IsLoopback() {
				continue
			}
			if ru.Src.IsLinkLocalUnicast() {
				continue
			}
			if ru.Dst.IP.IsLinkLocalUnicast() {
				continue
			}
			if ru.Dst.IP.IsInterfaceLocalMulticast() {
				continue
			}
			if ru.Dst.IP.IsLinkLocalMulticast() {
				continue
			}
			er := &exportRoute{
				Type:     ru.Type,
				Dst:      ru.Dst,
				Gw:       ru.Gw,
				Priority: ru.Priority,
			}
			localRouteUpdate <- er
		}
	}()

	listener, err := net.Listen("tcp", ":"+strconv.Itoa(*port))
	if err != nil {
		log.Error("Failed to setup listener")
		log.Error(err)
		return
	}

	peerCh := make(chan *net.Conn)
	// Listen for new inbound connetions
	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				log.Error("Failed to accept listener")
				log.Error(err)
				continue
			}
			peerCh <- &c
		}
	}()

	peerConnected := make(chan string)
	disconnectPeer := make(chan string)
	// Initiate outbound connections from discovered peers
	go func() {
		peers := make(map[string]struct{})
		for {
			select {
			case p := <-connectPeer:
				if _, ok := peers[p]; !ok {
					c, err := net.Dial("tcp", p)
					if err != nil {
						log.Error("Failed to dial")
						log.Error(err)
						continue
					}
					peerCh <- &c
				}
			case p := <-peerConnected:
				peers[p] = struct{}{}
				log.Debugf("Current peers: %v", peers)
			case p := <-disconnectPeer:
				delete(peers, p)
				log.Debugf("Current peers: %v", peers)
			}
		}
	}()

	// Handle new peers joining
	var subscribers []*subscriber
	addSubscriber := make(chan *subscriber)
	go func() {
		c := *<-peerCh

		// Send hello on all new tcp connections
		e := gob.NewEncoder(c)
		err := e.Encode(helloMsg)
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
		peerConnected <- h.ListenAddr

		// Handle messages with this peer
		s := &subscriber{
			cb:  make(chan *exportRoute),
			del: make(chan struct{}),
		}
		addSubscriber <- s

		// die gracefully
		sendDie := make(chan struct{})
		recvDie := make(chan struct{})
		go func() {
			select {
			case _ = <-sendDie:
				break
			case _ = <-recvDie:
				break
			}
			close(s.del)
			disconnectPeer <- c.RemoteAddr().String()
			err := delAllRoutesVia(c.RemoteAddr())
			if err != nil {
				log.Errorf("Failed to delete all routes via %v", c.RemoteAddr())
			}
		}()

		// Send updates to this peer
		go func() {
			e := gob.NewEncoder(c)
			srcAddr := c.LocalAddr().(*net.TCPAddr).IP
			for {
				r := <-s.cb
				if r == nil { // r is closed on subscriber
					return
				}
				if (r.Dst.IP.To4 == nil) != (srcAddr.To4 == nil) {
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
		go func() {
			d := gob.NewDecoder(c)
			for {
				er := &exportRoute{}
				err := d.Decode(er)
				if err != nil {
					log.Error("Failed to decode route update")
					log.Error(err)
					close(recvDie)
					return
				}

				log.Debugf("Recieved update %v from %v", *er, c.RemoteAddr())
				err = processRoute(er, c.RemoteAddr())
				if err != nil {
					log.Errorf("Failed to process route: %v", *er)
					continue
				}
			}
		}()
	}()

	delSubscriber := make(chan int)
	// Manage listening peers
	for {
		select {
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
			go func() {
				_ = <-s.del
				close(s.cb)
				delSubscriber <- i
			}()
		case r := <-localRouteUpdate:
			for _, s := range subscribers {
				select {
				// just in case a del is pending
				case _ = <-s.del:
					break
				default:
					s.cb <- r
				}
			}
		}
	}
}
