// Command ghfn-worker is a ConfigHub function worker whose
// function set is sourced from a GitHub repository: each subdirectory under
// functions/ in the repo contributes one function, implemented by an
// executable file alongside a manifest.yaml describing the signature.
//
// See internal/protocol for the wire contract between the worker and the
// scripts it dispatches to.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/confighub/sdk/configkit/k8skit"
	"github.com/confighub/sdk/core/function/api"
	"github.com/confighub/sdk/core/function/executor"
	"github.com/confighub/sdk/core/function/handler"
	"github.com/confighub/sdk/core/third_party/gaby"
	"github.com/confighub/sdk/core/worker"
	"github.com/confighub/sdk/core/workerapi"

	"github.com/jesperfj/ghfn-worker/internal/gitsync"
	"github.com/jesperfj/ghfn-worker/internal/manifest"
	"github.com/jesperfj/ghfn-worker/internal/registry"
	"github.com/jesperfj/ghfn-worker/internal/scriptexec"
)

type config struct {
	WorkerID     string
	WorkerSecret string
	ConfigHubURL string

	RepoURL    string
	RepoBranch string
	RepoDir    string
	Token      string

	// LocalRepoPath, when set, bypasses git entirely and treats the path as
	// the already-checked-out repo root. Enables a local edit-sync-restart
	// loop without round-tripping through GitHub. Mutually exclusive with
	// REPO_URL (LOCAL_REPO_PATH wins if both are set).
	LocalRepoPath string

	DefaultToolchain workerapi.ToolchainType
}

// repoDir returns the directory the worker should scan for manifests.
// In local mode that's LocalRepoPath as-is; otherwise it's RepoDir (the
// clone destination).
func (c *config) repoDir() string {
	if c.LocalRepoPath != "" {
		return c.LocalRepoPath
	}
	return c.RepoDir
}

func (c *config) localMode() bool { return c.LocalRepoPath != "" }

func loadConfig() (*config, error) {
	c := &config{
		WorkerID:         os.Getenv("CONFIGHUB_WORKER_ID"),
		WorkerSecret:     os.Getenv("CONFIGHUB_WORKER_SECRET"),
		ConfigHubURL:     os.Getenv("CONFIGHUB_URL"),
		RepoURL:          os.Getenv("REPO_URL"),
		RepoBranch:       getenvDefault("REPO_BRANCH", "main"),
		RepoDir:          getenvDefault("REPO_DIR", "/var/lib/confighub/function-repo"),
		Token:            firstNonEmpty(os.Getenv("GITHUB_TOKEN"), os.Getenv("REPO_TOKEN")),
		LocalRepoPath:    os.Getenv("LOCAL_REPO_PATH"),
		DefaultToolchain: workerapi.ToolchainType(getenvDefault("DEFAULT_TOOLCHAIN", string(workerapi.ToolchainKubernetesYAML))),
	}
	var missing []string
	if c.WorkerID == "" {
		missing = append(missing, "CONFIGHUB_WORKER_ID")
	}
	if c.WorkerSecret == "" {
		missing = append(missing, "CONFIGHUB_WORKER_SECRET")
	}
	if c.ConfigHubURL == "" {
		missing = append(missing, "CONFIGHUB_URL")
	}
	if c.RepoURL == "" && c.LocalRepoPath == "" {
		missing = append(missing, "REPO_URL or LOCAL_REPO_PATH")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required env vars: %s", strings.Join(missing, ", "))
	}
	return c, nil
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	ctx := context.Background()

	// Initial sync (skipped in local mode).
	if cfg.localMode() {
		log.Printf("repo: local mode, scanning %s", cfg.LocalRepoPath)
	} else {
		syncRes, err := gitsync.EnsureRepo(ctx, gitsync.Options{
			RepoURL: cfg.RepoURL,
			Branch:  cfg.RepoBranch,
			Dir:     cfg.RepoDir,
			Token:   cfg.Token,
		})
		if err != nil {
			log.Fatalf("git sync: %v", err)
		}
		log.Printf("repo synced: branch=%s sha=%s cloned=%v", cfg.RepoBranch, syncRes.CommitSHA, syncRes.Cloned)
	}

	// Build registry from manifests.
	reg := registry.New()
	entries, err := loadEntries(cfg)
	if err != nil {
		log.Fatalf("scan manifests: %v", err)
	}
	reg.Replace(entries)
	log.Printf("registered %d functions: %s", len(entries), strings.Join(reg.Names(), ", "))

	// Build executor with the toolchains used by the loaded entries.
	exec := executor.NewEmptyExecutor()
	for _, tc := range collectToolchains(entries) {
		provider, err := toolchainProvider(tc)
		if err != nil {
			return // toolchainProvider returned a fatal log already
		}
		// captureSignatures=true so the toolchain's auto-registered functions
		// (compute-mutations etc.) appear in the signature registry that
		// gets advertised to ConfigHub.
		exec.RegisterToolchain(provider, true)
	}

	// Register repo functions.
	for fullName, entry := range entries {
		sig := entry.Manifest.ToSignature(fullName)
		if err := exec.RegisterFunction(entry.Toolchain, handler.FunctionRegistration{
			FunctionSignature: sig,
			Function:          scriptexec.Make(fullName, reg.Lookup()),
		}); err != nil {
			log.Fatalf("register %s: %v", fullName, err)
		}
	}

	// Register the reserved refresh-function-repo builtin under the default
	// toolchain. Function names are scoped per-worker on the server side, so
	// no prefix is needed to avoid collision with other workers.
	const refreshName = "refresh-function-repo"
	if err := exec.RegisterFunction(cfg.DefaultToolchain, handler.FunctionRegistration{
		FunctionSignature: refreshSignature(refreshName),
		Function:          makeRefreshFn(cfg),
	}); err != nil {
		log.Fatalf("register %s: %v", refreshName, err)
	}
	log.Printf("registered builtin: %s (toolchain=%s)", refreshName, cfg.DefaultToolchain)

	// DEBUG: dump the full set of advertised functions so we can confirm
	// what gets sent to ConfigHub at connect time.
	for tc, sigs := range exec.RegisteredFunctions() {
		names := make([]string, 0, len(sigs))
		for n := range sigs {
			names = append(names, n)
		}
		sort.Strings(names)
		log.Printf("DEBUG advertised toolchain=%s functions=%v", tc, names)
	}

	// SIGHUP triggers the same restart path as the refresh function —
	// primarily for local dev where there's no Unit handy to invoke
	// refresh-function-repo against.
	startSighupRestarter()

	// Connect.
	connector, err := worker.NewConnector(worker.ConnectorOptions{
		WorkerID:         cfg.WorkerID,
		WorkerSecret:     cfg.WorkerSecret,
		ConfigHubURL:     cfg.ConfigHubURL,
		FunctionExecutor: exec,
	})
	if err != nil {
		log.Fatalf("connector: %v", err)
	}
	if err := connector.Start(); err != nil {
		log.Fatalf("connector start: %v", err)
	}
}

// loadEntries scans the repo and produces the fullName -> Entry map.
func loadEntries(cfg *config) (map[string]*scriptexec.Entry, error) {
	loaded, err := manifest.ScanRepo(cfg.repoDir())
	if err != nil {
		return nil, err
	}
	out := make(map[string]*scriptexec.Entry, len(loaded))
	for _, l := range loaded {
		fullName := l.Manifest.Name
		if fullName == "refresh-function-repo" {
			return nil, fmt.Errorf("function name %q is reserved by the worker", l.Manifest.Name)
		}
		if _, dup := out[fullName]; dup {
			return nil, fmt.Errorf("duplicate function name %q", fullName)
		}
		out[fullName] = &scriptexec.Entry{
			FullName:  fullName,
			Toolchain: l.Manifest.ResolveToolchain(cfg.DefaultToolchain),
			Manifest:  l.Manifest,
			Dir:       l.Dir,
			ExecPath:  l.ExecPath,
		}
	}
	return out, nil
}

// collectToolchains returns the unique set of toolchain types declared by
// the loaded entries, in deterministic order.
func collectToolchains(entries map[string]*scriptexec.Entry) []workerapi.ToolchainType {
	seen := map[workerapi.ToolchainType]bool{}
	for _, e := range entries {
		seen[e.Toolchain] = true
	}
	out := make([]workerapi.ToolchainType, 0, len(seen)+1)
	for tc := range seen {
		out = append(out, tc)
	}
	// Always include Kubernetes/YAML so the refresh builtin can attach to
	// it even when no repo function declares a toolchain.
	if !seen[workerapi.ToolchainKubernetesYAML] {
		out = append(out, workerapi.ToolchainKubernetesYAML)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// toolchainProvider returns the SDK ResourceProvider for the given toolchain.
// v1 supports Kubernetes/YAML; extending this map is how new toolchains get
// added.
func toolchainProvider(tc workerapi.ToolchainType) (handler.ToolchainProvider, error) {
	switch tc {
	case workerapi.ToolchainKubernetesYAML:
		return k8skit.NewK8sResourceProvider(), nil
	default:
		log.Fatalf("toolchain %q is not supported by this worker; add a provider in toolchainProvider()", tc)
		return nil, fmt.Errorf("unsupported toolchain")
	}
}

// refreshSignature describes the reserved refresh-function-repo function.
// Invoking it (or sending SIGHUP) triggers a clean process exit;
// kubelet restarts the container, which re-pulls the repo (or rescans the
// local mount) and re-advertises the resulting function set to ConfigHub.
//
// In-process refresh would be cheaper, but the SDK pins a worker's signature
// registry at connection time and exposes no API to update it mid-stream. So
// "refresh" really has to mean "reconnect," and the simplest reconnect is a
// container restart.
func refreshSignature(fullName string) api.FunctionSignature {
	return api.FunctionSignature{
		FunctionName: fullName,
		Description: "Triggers a worker restart so the function repo is re-pulled and the resulting signature set is re-advertised to ConfigHub. " +
			"The function returns immediately; the actual exit is delayed briefly so the response can flush back to the caller.",
		Hermetic:              false,
		Idempotent:            true,
		FunctionType:          api.FunctionTypeCustom,
		AffectedResourceTypes: []api.ResourceType{api.ResourceTypeAny},
		OutputInfo: &api.FunctionOutput{
			ResultName:  "restart-info",
			Description: "Notice that a restart was scheduled.",
			OutputType:  api.OutputTypeOpaque,
		},
	}
}

// restartGracePeriod is the delay between deciding to restart and calling
// os.Exit. Two reasons it exists:
//
//   - When the trigger is the refresh function, the SSE response carrying
//     our return value still has to flush back to ConfigHub. Exiting
//     immediately can truncate it.
//   - It absorbs any in-flight invocations that are seconds away from
//     completing. Anything longer than the grace period is killed; that's
//     the trade-off for picking a small number.
//
// Anything in the 1–3s range is reasonable. 2s is comfortably above
// observed round-trip times to hub.confighub.com without making operators
// feel like the call hung.
const restartGracePeriod = 2 * time.Second

// restartOnce guards triggerRestart so a flurry of refresh invocations or
// signals doesn't queue up multiple AfterFunc timers.
var restartOnce sync.Once

func triggerRestart(reason string) {
	restartOnce.Do(func() {
		log.Printf("scheduling container restart in %s: %s", restartGracePeriod, reason)
		time.AfterFunc(restartGracePeriod, func() {
			log.Printf("exiting now; kubelet will restart the container")
			os.Exit(0)
		})
	})
}

func makeRefreshFn(cfg *config) handler.FunctionImplementation {
	return func(fArgs handler.FunctionImplementationArguments) (gaby.Container, any, error) {
		triggerRestart("refresh-function-repo invoked")
		out := map[string]any{
			"restarting":    true,
			"grace_seconds": int(restartGracePeriod.Seconds()),
			"local_mode":    cfg.localMode(),
			"message":       "container will exit shortly; on restart the worker re-scans the function repo and re-advertises to ConfigHub",
		}
		return fArgs.ParsedData, out, nil
	}
}

// startSighupRestarter triggers the same restart path as the refresh
// function, but in response to SIGHUP. Used for local dev where there's no
// Unit handy to invoke the function against:
//
//	kubectl exec <pod> -- kill -HUP 1
func startSighupRestarter() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGHUP)
	go func() {
		for range sigCh {
			triggerRestart("SIGHUP received")
		}
	}()
}

func getenvDefault(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
