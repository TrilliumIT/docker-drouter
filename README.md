A proof of concept route sharing program. Devepoped for integration into drouter.

Listens on a multicast address for peers. When peers are detected, a tcp connection is opened between the peers.

When the routing table is updated, this update is broadcasted to all connected peers.

Using standard tcp dial, so bolting on TLS should be pretty easy.
