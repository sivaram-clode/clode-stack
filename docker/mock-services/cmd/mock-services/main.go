// Command mock-services is the local stack's unified mock server. On one HTTP
// server it serves every mocked dependency as a route group, against the host
// docker daemon:
//
//	aws        — a subset of the EC2 wire protocol: RunInstances → docker run,
//	             Stop/Start/Terminate → docker stop/start/rm (brahmi's aramb-vm).
//	narnia     — jumbo's deployer facade: a deployment batch pulls the service
//	             config back from jumbo, runs/stops the container with the k8s
//	             DNS aliases, and posts the terminal status callback.
//	baghira    — pod status: GET /api/v1/replicas reports a service's health from
//	             live container state (pool-manager promotes warm agents on it).
//	composio   — a Postgres-backed mock of the Composio API for toolkit-proxy.
//	oauth-mock — Google/GitHub OAuth authorize/token/userinfo for CDP sign-in.
//
// Together they let the local stack run the full jumbo→(narnia)→(baghira)
// deploy path without any real k8s (no narnia, narnia-workers, argocd, baghira,
// or baghira-proxy). jumbo stays the book-keeper; this binary is the "cluster".
package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/sivaram-clode/mock-services/internal/client/jumbo"
	"github.com/sivaram-clode/mock-services/internal/config"
	"github.com/sivaram-clode/mock-services/internal/deploy"
	"github.com/sivaram-clode/mock-services/internal/mock/aws"
	"github.com/sivaram-clode/mock-services/internal/mock/baghira"
	"github.com/sivaram-clode/mock-services/internal/mock/narnia"
	"github.com/sivaram-clode/mock-services/internal/server"
)

func main() {
	// Sub-second precision so cold-start timing reads straight from the stream.
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	cfg := config.Load()

	// The aws group owns the docker client (dial + ping + rehydrate); the
	// deployer shares it so every group drives one daemon connection.
	awsMock, err := aws.New(aws.Config{
		DockerSocket:       cfg.DockerSocket,
		Network:            cfg.Network,
		EntrypointOverride: cfg.EntrypointOverride,
		PullPolicy:         cfg.PullPolicy,
	})
	if err != nil {
		log.Fatalf("mock-services: init: %v", err)
	}
	defer func() { _ = awsMock.Close() }()

	dep := deploy.New(awsMock.Docker(), cfg.Network, cfg.PullPolicy)
	app := server.New(awsMock, narnia.New(dep, jumbo.New(cfg.JumboBaseURL)), baghira.New(dep))

	go func() {
		log.Printf("mock-services: listening on %s (network=%s, jumbo=%s)", cfg.Addr, cfg.Network, cfg.JumboBaseURL)
		if err := app.Listen(cfg.Addr); err != nil {
			log.Fatalf("mock-services: serve: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("mock-services: shutting down")
	_ = app.Shutdown()
}
