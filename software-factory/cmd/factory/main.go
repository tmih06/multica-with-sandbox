package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"software-factory/internal/factory"
)

func main() {
	addr := flag.String("addr", envOr("FACTORY_ADDR", ":8080"), "HTTP listen address")
	namespace := flag.String("namespace", envOr("FACTORY_NAMESPACE", "factory"), "Kubernetes namespace for factory Sandboxes")
	image := flag.String("image", envOr("FACTORY_IMAGE", "ghcr.io/tmih06/ompig-slim-workspace:v0.1.5"), "workspace image")
	kubectl := flag.String("kubectl", envOr("KUBECTL", "kubectl"), "kubectl executable")
	timeoutMinutes := flag.Int("timeout-minutes", envInt("FACTORY_TIMEOUT_MINUTES", 30), "default maximum runtime per ticket")
	capacity := flag.Int("capacity", envInt("FACTORY_CAPACITY", 3), "maximum concurrent Sandbox runs")
	multicaURL := flag.String("multica-url", envOr("MULTICA_URL", envOr("MULTICA_SERVER_URL", "")), "Multica server URL; enables the Sandbox runtime bridge")
	multicaToken := flag.String("multica-token", envOr("MULTICA_TOKEN", ""), "Multica personal access token for the Sandbox runtime bridge")
	multicaWorkspace := flag.String("multica-workspace-id", envOr("MULTICA_WORKSPACE_ID", ""), "Multica workspace ID for the Sandbox runtime bridge")
	multicaRepository := flag.String("multica-repository", envOr("MULTICA_REPOSITORY", ""), "repository URL used by Sandbox tasks when the claim does not include one")
	multicaDaemonID := flag.String("multica-daemon-id", envOr("MULTICA_DAEMON_ID", "factory-sandbox"), "stable daemon identity registered in Multica")
	multicaProvider := flag.String("multica-provider", envOr("MULTICA_PROVIDER", "omp"), "Multica runtime provider registered by the Sandbox bridge")
	multicaAgentName := flag.String("multica-agent-name", envOr("MULTICA_AGENT_NAME", "Sandbox Builder"), "agent name created for each Sandbox slot")
	multicaAgentModel := flag.String("multica-agent-model", envOr("MULTICA_AGENT_MODEL", "deploy/deepseek-ai/deepseek-v4.1-flash-1017"), "model configured for automatically created Sandbox agents")
	multicaAgentThinking := flag.String("multica-agent-thinking", envOr("MULTICA_AGENT_THINKING", "medium"), "thinking level for automatically created Sandbox agents")
	multicaAgentInstructions := flag.String("multica-agent-instructions", envOr("MULTICA_AGENT_INSTRUCTIONS", "Work on the assigned issue. Make the requested changes, run relevant tests, and report the result."), "instructions for automatically created Sandbox agents")
	multicaPoll := flag.Duration("multica-poll-interval", envDuration("MULTICA_POLL_INTERVAL", 5*time.Second), "Multica task claim poll interval")
	brokerURL := flag.String("broker-url", envOr("FACTORY_BROKER_URL", ""), "private ompig-slim broker origin")
	brokerSecret := flag.String("broker-token-secret", envOr("FACTORY_BROKER_TOKEN_SECRET", ""), "Kubernetes Secret containing the broker token")
	databasePath := flag.String("db", envOr("FACTORY_DB", "factory.db"), "SQLite database path")
	flag.Parse()

	runner := factory.NewSandboxRunner(*kubectl, *namespace, *image, time.Duration(*timeoutMinutes)*time.Minute)
	runner.SetCapacity(*capacity)
	runner.BrokerURL = *brokerURL
	runner.BrokerTokenSecret = *brokerSecret
	runner.MulticaURL = *multicaURL
	runner.MulticaWorkspace = *multicaWorkspace
	store, err := factory.OpenSQLiteStore(*databasePath)
	if err != nil {
		log.Fatalf("open factory database: %v", err)
	}
	defer store.Close()
	server := factory.NewServerWithStoreAndCapacity(runner, store, *capacity)
	if err := server.Reconcile(context.Background()); err != nil {
		log.Fatalf("reconcile factory runs: %v", err)
	}
	httpServer := &http.Server{Addr: *addr, Handler: server.Handler(), ReadHeaderTimeout: 5 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *multicaToken != "" || *multicaWorkspace != "" {
		if *multicaURL == "" || *multicaToken == "" || *multicaWorkspace == "" {
			log.Fatal("multica bridge requires MULTICA_SERVER_URL, MULTICA_TOKEN, and MULTICA_WORKSPACE_ID")
		}
		bridge := factory.NewMulticaTaskBridge(
			factory.NewMulticaClient(*multicaURL, *multicaToken),
			*multicaWorkspace, *multicaDaemonID, *multicaProvider, *multicaRepository,
			*multicaPoll, *capacity, runner,
		)
		bridge.AgentName = *multicaAgentName
		bridge.AgentModel = *multicaAgentModel
		bridge.AgentThinking = *multicaAgentThinking
		if bridge.AgentModel == "none" {
			bridge.AgentModel = ""
			bridge.AgentThinking = ""
		}
		bridge.AgentInstructions = *multicaAgentInstructions
		go func() {
			if err := bridge.Run(ctx); err != nil && ctx.Err() == nil {
				log.Printf("multica bridge stopped: %v", err)
			}
		}()
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	log.Printf("factory listening on %s namespace=%s image=%s", *addr, *namespace, *image)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envDuration(name string, fallback time.Duration) time.Duration {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		log.Printf("ignoring invalid %s=%q", name, value)
		return fallback
	}
	return parsed
}

func envInt(name string, fallback int) int {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 {
		log.Printf("ignoring invalid %s=%q", name, value)
		return fallback
	}
	return parsed
}
