package main

import (
	"encoding/json"
	log "github.com/Sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"net"
	"strconv"
)

type subscriber struct {
	cb  chan string
	del chan struct{}
}

func startPeer(connectPeer <-chan string, hc <-chan []byte) {
	helloMsg := <-hc

	localRouteUpdate := make(chan string)

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
			ruj, err := json.Marshal(ru)
			if err != nil {
				log.Errorf("Failed to marshal: %v", ru)
				log.Error(err)
			}
			localRouteUpdate <- string(ruj)
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
		for {
			c := *<-peerCh

			// Send hello on all new tcp connections
			_, err = c.Write(helloMsg)
			if err != nil {
				log.Error("Failed to send tcp hello on connect")
				log.Error(err)
				continue
			}

			// get corresponding hello from the other side
			b := make([]byte, 512)
			n, err := c.Read(b)
			if err != nil {
				log.Errorf("Failed to read tcp hello")
				log.Error(err)
				return
			}

			h := &hello{}
			err = json.Unmarshal(b[:n], h)
			if err != nil {
				log.Error("Unable to unmarshall tcp hello")
				log.Errorf("Data: %v", string(b[:n]))
				log.Error(err)
				return
			}

			log.Debugf("hello recieved via tcp: %v", h)
			peerConnected <- h.ListenAddr

			// Handle messages with this peer
			s := &subscriber{
				cb:  make(chan string),
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
			}()

			// Send updates to this peer
			go func() {
				for {
					r := <-s.cb
					if r == "" { // r is closed on subscriber
						return
					}
					_, err := c.Write([]byte(r))
					if err != nil {
						log.Error("Failed to write to c")
						log.Error(err)
						close(sendDie)
						return
					}
				}
			}()

			// Recieve messages from this peer
			go func() {
				for {
					b := make([]byte, 512)
					n, err := c.Read(b)
					if err != nil {
						log.Error("Failed to read from c")
						log.Error(err)
						close(recvDie)
						return
					}
					// TODO Process external message
					log.Infof("Recieved update %v from %v", string(b[:n]), c.RemoteAddr())
				}
			}()
		}
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
