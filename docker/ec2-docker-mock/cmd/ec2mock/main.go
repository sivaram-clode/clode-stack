// ec2mock is a minimal AWS EC2 API server that translates a small subset of the
// EC2 wire protocol into docker container operations. Point an aws-sdk-go-v2
// EC2 client at http://<host>:4566 (or override via AWS_ENDPOINT_URL_EC2) and
// launching an "EC2 instance" turns into `docker create + start` on the host
// docker daemon. Stop → docker stop, Start → docker start, Hibernate → docker
// pause, Terminate → docker rm -f.
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/sivaram-clode/ec2-docker-mock/internal/mock"
)

func main() {
	// Sub-second precision on the default log prefix so cold-start timing is
	// readable straight from the stream — one timestamp per line, from the log
	// package itself.
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	addr := flag.String("addr", ":8080", "listen address (host:port)")
	dockerSock := flag.String("docker-socket", "", "docker daemon socket (empty = use DOCKER_HOST env or /var/run/docker.sock)")
	network := flag.String("network", "bridge", "docker network launched containers attach to")
	entrypointOverride := flag.String("entrypoint-override", "", "optional /path/to/entrypoint.sh — if set, containers use this instead of the image's default entrypoint (useful when the image expects systemd)")
	pullPolicy := flag.String("pull-policy", "IfNotPresent", "image pull policy: IfNotPresent (default) | Always | Never")
	flag.Parse()

	m, err := mock.New(mock.Config{
		DockerSocket:       *dockerSock,
		Network:            *network,
		EntrypointOverride: *entrypointOverride,
		PullPolicy:         *pullPolicy,
	})
	if err != nil {
		log.Fatalf("ec2mock: init: %v", err)
	}
	defer func() { _ = m.Close() }()

	srv := &http.Server{Addr: *addr, Handler: m}
	go func() {
		log.Printf("ec2mock: listening on %s (docker network=%s)", *addr, *network)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("ec2mock: serve: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("ec2mock: shutting down")
	_ = srv.Close()
}
