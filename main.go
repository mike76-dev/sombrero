package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mike76-dev/sombrero/api"
	"github.com/mike76-dev/sombrero/client"
	"github.com/mike76-dev/sombrero/ntlm"
	"github.com/mike76-dev/sombrero/smb2"
	"github.com/mike76-dev/sombrero/stores"
	"github.com/mike76-dev/sombrero/web"
	sdk "go.sia.tech/siastorage"
)

const version = "2.1.0"

var storesDir = flag.String("dir", ".", "directory for storing persistent data")

// newHTTPHandler wires the API and the web UI together. The API lives under
// /api, behind the password and the ratelimiter. The web UI is served from
// the root: it holds nothing secret, and asking for the password itself
// beats leaving it to the browser's basic auth prompt.
func newHTTPHandler(ctx context.Context, a http.Handler, password string) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/api/", http.StripPrefix("/api", api.Ratelimit(ctx)(api.BasicAuth(password)(a))))
	mux.Handle("/", web.Handler())
	return mux
}

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

	// The API administers the whole server, so it must not be reachable
	// without a password.
	if cfg.API.Password == "" {
		log.Fatal("no API password set: add one to the `api` section of sombrero.yml")
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
	lAPI, err := net.Listen("tcp", cfg.API.Address)
	if err != nil {
		log.Fatal(err)
	}
	defer lAPI.Close()
	a := api.NewAPI(ctx, db, server, cfg.Indexd, cfg.Mode)
	apiSrv := &http.Server{Handler: newHTTPHandler(ctx, a, cfg.API.Password)}
	go apiSrv.Serve(lAPI)
	log.Printf("API and web UI: listening at http://%s ...\n", lAPI.Addr())

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

		// Each client drains what it has in flight before giving up on it, so
		// they are closed side by side rather than one shutdown timeout after
		// another.
		var wg sync.WaitGroup
		closeClient := func(c client.Client) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := c.Close(); err != nil {
					log.Printf("failed to close client: %v", err)
				}
			}()
		}
		for _, share := range shares {
			if share.client != nil { // renterd share
				closeClient(share.client)
			}
			for _, conn := range share.indexdConns { // indexd share
				closeClient(conn.client)
			}
		}
		wg.Wait()

		apiSrv.Close()
		lAPI.Close()
		l.Close()

		// Only now, with nothing left that needs the store: the clients spend
		// their shutdown recording, requeueing and unpinning what they were in
		// the middle of, and closing the store cuts all of that off.
		db.Close()

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
				conn.Close()
				continue
			}

			// Start serving the connection.
			go func() {
				server.mu.Lock()
				enabled := server.enabled
				server.mu.Unlock()
				if !enabled {
					conn.Close()
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
