package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mike76-dev/sombrero/api"
	"github.com/mike76-dev/sombrero/ntlm"
	"github.com/mike76-dev/sombrero/smb2"
	"github.com/mike76-dev/sombrero/stores"
	sdk "go.sia.tech/siastorage"
)

const version = "2.1.0"

var storesDir = flag.String("dir", ".", "directory for storing persistent data")

func main() {
	log.Printf("Starting Sombrero v%s...\n", version)

	// Parse command-line args.
	flag.Parse()
	dir, err := filepath.Abs(*storesDir)
	if err != nil {
		panic(err)
	}

	// Read the config file.
	cfg, err := stores.ReadConfig(dir)
	if err != nil {
		panic(err)
	}

	if cfg.Mode == stores.ModeNormal {
		if len(cfg.Database.Password) < 4 {
			log.Fatal("database password too short")
		}

		if cfg.Indexd.SeedPhrase == "" {
			// Generate a new seed phrase.
			cfg.Indexd.SeedPhrase = sdk.NewSeedPhrase()
			if err := stores.SaveConfig(cfg, dir); err != nil {
				log.Fatalf("failed to generate seed phrase: %v", err)
			}
			log.Printf("Generated seed phrase: %s", cfg.Indexd.SeedPhrase)
		}
	} else {
		log.Println("Running in Lite mode: only renterd shares are supported")
	}

	// Start a thread to watch for the stop signal.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Open the store: a SQL database in the Normal mode,
	// a JSON file in the Lite mode.
	var db stores.Store
	if cfg.Mode == stores.ModeLite {
		db, err = stores.NewJSONStore(dir)
	} else {
		db, err = stores.NewStore(ctx, cfg.Database)
	}
	if err != nil {
		panic(err)
	}
	defer db.Close()

	// Start listening on the SMB port 445.
	l, err := net.Listen("tcp", ":445")
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("SMB: listening at %s ...\n", l.Addr())
	defer l.Close()

	// Start the SMB server.
	server := newServer(ctx, l, db, cfg.Debug, cfg.Indexd)
	server.applyCapabilities()
	db.WithShares(server)

	// Start the API server.
	lAPI, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.API.Port))
	if err != nil {
		log.Fatal(err)
	}
	defer lAPI.Close()
	a := api.NewAPI(ctx, db, cfg.Indexd, cfg.Mode, server.Stats)
	apiSrv := &http.Server{Handler: api.BasicAuth(cfg.API.Password)(a)}
	go apiSrv.Serve(lAPI)
	log.Printf("API: listening at %s ...\n", lAPI.Addr())

	go func() {
		func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-time.After(10 * time.Minute):
					// Reset the abuse protection.
					server.mu.Lock()
					server.connectionCount = make(map[string]int)
					cl := make([]*connection, 0, len(server.connectionList))
					for _, cn := range server.connectionList {
						cl = append(cl, cn)
					}
					server.mu.Unlock()

					// Drop unused connections.
					for _, cn := range cl {
						if cn.isStale() {
							server.closeConnection(cn)
						}
					}
				}
			}
		}()

		log.Println("Received interrupt signal, shutting down...")
		server.mu.Lock()
		server.enabled = false
		conns := make([]*connection, 0, len(server.connectionList))
		shares := make([]*share, 0, len(server.shareList))
		for _, c := range server.connectionList {
			conns = append(conns, c)
		}
		for _, s := range server.shareList {
			shares = append(shares, s)
		}
		server.mu.Unlock()

		for _, connection := range conns {
			log.Printf("Closing connection from client %s\n", connection.clientName)
			connection.conn.Close()
			connection.once.Do(func() { close(connection.closeChan) })
		}

		for _, share := range shares {
			if share.client != nil { // renterd share
				share.client.Close()
			}
			for _, conn := range share.indexdConns { // indexd share
				conn.client.Close()
			}
		}

		apiSrv.Close()
		lAPI.Close()
		l.Close()
		os.Exit(0)
	}()

	for {
		if conn, err := l.Accept(); err == nil {
			// Check if the remote host is on the ban list.
			host, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
			banned, _, err := db.IsBanned(host)
			if err != nil {
				log.Printf("Error checking ban status for host %s: %v", host, err)
			} else if banned {
				conn.Close()
				continue
			}

			// Ban the remote host if it forms too many connections.
			server.mu.Lock()
			num := server.connectionCount[host]
			server.connectionCount[host] = num + 1
			server.mu.Unlock()
			if num >= cfg.MaxConnections {
				server.blockHost(host, "too many connections")
				log.Printf("Blocked host %s for too many connections (%d)\n", host, num)
			}

			// Start serving the connection.
			go func() {
				server.mu.Lock()
				enabled := server.enabled
				server.mu.Unlock()
				if !enabled {
					return
				}

				log.Println("Incoming connection from", conn.RemoteAddr())
				c := server.newConnection(conn)
				c.ntlmServer = ntlm.NewServer("SERVER", "", db)

				for {
					msg, err := readMessage(conn)
					if err != nil && strings.Contains(err.Error(), "EOF") {
						time.Sleep(100 * time.Millisecond)
						continue
					} else if err != nil {
						if !strings.Contains(err.Error(), "use of closed network connection") {
							log.Println("Error reading message:", err)
						}
						server.closeConnection(c)
						return
					}

					server.mu.Lock()
					server.stats.BytesRcvd += uint64(len(msg))
					server.mu.Unlock()

					if err := c.acceptRequest(msg); err != nil {
						log.Println("couldn't accept request:", err)
						server.closeConnection(c)
						if errors.Is(err, smb2.ErrWrongProtocol) {
							// Ban the remote host if it keeps sending SMB requests after receiving
							// an SMB2_NEGOTIATE response.
							server.blockHost(host, "old protocol")
							log.Printf("Blocked host %s for using old protocol\n", host)
						}
						return
					}
				}
			}()
		}
	}
}
