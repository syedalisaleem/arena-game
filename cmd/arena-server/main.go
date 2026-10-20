// Command arena-server runs the authoritative arena game server and serves
// the embedded web client on the same port.
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/ali/arena-game/server"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address (host:port)")
	flag.Parse()

	hub := server.NewHub()
	stop := make(chan struct{})
	defer close(stop)
	go hub.Run(stop)

	mux := http.NewServeMux()
	mux.Handle("GET /ws", http.HandlerFunc(hub.ServeWS))
	mux.Handle("GET /", server.FileServer())
	mux.Handle("GET /healthz", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))

	srv := &http.Server{Addr: *addr, Handler: mux}
	go func() {
		log.Printf("arena-server listening on %s", *addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("shutting down")
	_ = srv.Close()
}
