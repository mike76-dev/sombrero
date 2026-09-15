package main

import (
	"log"
	"net"
)

// blockHost adds a remote host to the bans store together with the provided reason.
func (s *server) blockHost(host, reason string) {
	log.Printf("Banning %s: %s", host, reason)

	if err := s.store.BanHost(host, reason); err != nil {
		log.Printf("Error banning host %s: %v", host, err)
	}

	var conns []*connection
	s.mu.Lock()
	for addr, c := range s.connectionList {
		h, _, err := net.SplitHostPort(addr)
		if err != nil {
			continue
		}
		if h == host {
			conns = append(conns, c)
		}
	}
	s.mu.Unlock()

	for _, c := range conns {
		s.closeConnection(c)
	}
}
