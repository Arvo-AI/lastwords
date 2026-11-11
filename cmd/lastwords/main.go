// Command lastwords saves what a container was doing right before Kubernetes
// killed it: memory snapshots before OOM kills, crash logs, core dumps.
//
//	lastwords agent          run the node agent (DaemonSet)
//	lastwords list           list captures across the cluster
//	lastwords show <id>      print a capture summary
//	lastwords get <id> <f>   download an evidence file
//	lastwords top            containers closest to their memory limit
//	lastwords mcp            MCP server for AI agents (stdio)
//
// Installed as kubectl-lastwords it works as `kubectl lastwords ...`.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/Arvo-AI/lastwords/internal/agent"
	"github.com/Arvo-AI/lastwords/internal/client"
	"github.com/Arvo-AI/lastwords/internal/core"
	"github.com/Arvo-AI/lastwords/internal/jvm"
	"github.com/Arvo-AI/lastwords/internal/logs"
	"github.com/Arvo-AI/lastwords/internal/mcpserver"
	"github.com/Arvo-AI/lastwords/internal/procfs"
)

const usage = `lastwords saves what a container was doing right before Kubernetes killed it.

Usage:
  lastwords list [flags]          List captures (OOM kills, memory pressure, crashes, core dumps, evictions)
  lastwords show <id>             Print a capture's summary
  lastwords get <id> <file>       Download an evidence file (-o to choose the path)
  lastwords top                   Containers closest to their memory limit right now
  lastwords mcp                   Serve the MCP tools for AI agents over stdio
  lastwords agent [flags]         Run the node agent (used by the DaemonSet)
  lastwords version

Global flags (list/show/get/top/mcp):
  --kubeconfig, --context         Cluster to talk to (default: current kubectl context)
  --lastwords-namespace           Namespace lastwords is installed in (default "lastwords")

Install:  helm install lastwords oci://ghcr.io/arvo-ai/charts/lastwords -n lastwords --create-namespace
Agents:   claude mcp add lastwords -- kubectl-lastwords mcp
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := splitCommand(os.Args[1:])
	var err error
	switch cmd {
	case "agent":
		err = runAgent(args)
	case "core-handler":
		// Invoked by the kernel through core_pattern. Never print to stdout.
		if herr := core.Handle(args, os.Stdin); herr != nil {
			os.Exit(1)
		}
		return
	case "list", "ls":
		err = runList(args)
	case "show":
		err = runShow(args)
	case "get":
		err = runGet(args)
	case "top":
		err = runTop(args)
	case "mcp":
		err = runMCP(args)
	case "version", "--version", "-v":
		fmt.Println("lastwords", agent.Version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "lastwords:", err)
		os.Exit(1)
	}
}

// splitCommand finds the subcommand even when global flags come first
// (kubectl lastwords --context prod list).
func splitCommand(argv []string) (string, []string) {
	valued := map[string]bool{"--kubeconfig": true, "--context": true, "--lastwords-namespace": true, "-kubeconfig": true, "-context": true, "-lastwords-namespace": true}
	var lead []string
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if !strings.HasPrefix(a, "-") || a == "-h" || a == "--help" || a == "-v" || a == "--version" {
			rest := append([]string{}, argv[i+1:]...)
			return a, append(lead, rest...)
		}
		lead = append(lead, a)
		if valued[a] && i+1 < len(argv) {
			lead = append(lead, argv[i+1])
			i++
		}
	}
	return "help", nil
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func runAgent(args []string) error {
	cfg := agent.Defaults()
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	fs.StringVar(&cfg.NodeName, "node-name", env("NODE_NAME", ""), "node this agent runs on (downward API)")
	fs.StringVar(&cfg.Cluster, "cluster", env("LASTWORDS_CLUSTER", ""), "cluster name for notifications and upload paths")
	fs.StringVar(&cfg.DataDir, "data-dir", cfg.DataDir, "directory for captures (hostPath)")
	fs.StringVar(&cfg.CgroupDir, "cgroup-dir", cfg.CgroupDir, "host cgroup mount")
	fs.StringVar(&cfg.ProcDir, "proc-dir", cfg.ProcDir, "host /proc (needs hostPID)")
	fs.StringVar(&cfg.LogDir, "log-dir", cfg.LogDir, "host /var/log/pods")
	fs.DurationVar(&cfg.Interval, "interval", cfg.Interval, "memory sampling interval")
	fs.Float64Var(&cfg.Threshold, "threshold", cfg.Threshold, "capture when working set reaches this fraction of the limit")
	fs.Float64Var(&cfg.LastGasp, "last-gasp", cfg.LastGasp, "second capture at this fraction (0 disables)")
	fs.BoolVar(&cfg.Predictive, "predictive", cfg.Predictive, "capture early when the limit will be hit within --predict-seconds")
	fs.Float64Var(&cfg.PredictSecs, "predict-seconds", cfg.PredictSecs, "seconds-to-limit that triggers an early capture")
	fs.Float64Var(&cfg.JVMGCHeadroom, "jvm-gc-headroom", cfg.JVMGCHeadroom, "share of the limit a JVM needs free for its next GC; JVM capture tiers are measured against limit*(1-headroom) (0 disables)")
	fs.DurationVar(&cfg.History, "history", cfg.History, "memory timeline kept per container")
	fs.DurationVar(&cfg.CaptureTimeout, "capture-timeout", cfg.CaptureTimeout, "time budget per capture")
	fs.BoolVar(&cfg.JVMThreadDump, "jvm-thread-dump", cfg.JVMThreadDump, "include Java thread dumps")
	fs.BoolVar(&cfg.AllowHeapDumps, "heap-dumps", cfg.AllowHeapDumps, "take full heap dumps for every JVM (otherwise only pods annotated lastwords.dev/heap-dump=true)")
	fs.Int64Var(&cfg.HeapDumpMaxBytes, "heap-dump-max-bytes", cfg.HeapDumpMaxBytes, "drop gzipped heap dumps larger than this")
	fs.IntVar(&cfg.GoPprofPort, "go-pprof-port", cfg.GoPprofPort, "default net/http/pprof port for Go programs")
	fs.BoolVar(&cfg.CaptureLogs, "logs", cfg.CaptureLogs, "save the log tail of containers that die")
	fs.IntVar(&cfg.LogLines, "log-lines", cfg.LogLines, "log lines to keep")
	fs.BoolVar(&cfg.CoreDumps, "core-dumps", cfg.CoreDumps, "capture core dumps (sets the node's kernel.core_pattern; needs privileged)")
	fs.StringVar(&cfg.CoreHostDir, "core-host-dir", "", "data dir as the host kernel sees it (default: --data-dir)")
	fs.Int64Var(&cfg.CoreMaxBytes, "core-max-bytes", cfg.CoreMaxBytes, "truncate core dumps above this size")
	fs.BoolVar(&cfg.RestoreCoreOnExit, "restore-core-pattern", cfg.RestoreCoreOnExit, "restore the previous core_pattern when the agent stops")
	fs.Int64Var(&cfg.MaxStoreBytes, "max-store-bytes", cfg.MaxStoreBytes, "disk budget for captures on this node")
	fs.DurationVar(&cfg.MaxAge, "max-age", cfg.MaxAge, "delete captures older than this")
	fs.StringVar(&cfg.ListenAddr, "listen", cfg.ListenAddr, "API and metrics address")
	fs.StringVar(&cfg.WebhookURL, "webhook-url", env("LASTWORDS_WEBHOOK_URL", ""), "POST every capture as JSON to this URL")
	fs.StringVar(&cfg.WebhookSecret, "webhook-secret", env("LASTWORDS_WEBHOOK_SECRET", ""), "sign webhook deliveries with HMAC-SHA256 (X-Lastwords-Signature)")
	fs.StringVar(&cfg.SlackWebhookURL, "slack-webhook-url", env("LASTWORDS_SLACK_WEBHOOK_URL", ""), "post captures to this Slack incoming webhook")
	notify := fs.String("notify-kinds", strings.Join(cfg.NotifyKinds, ","), "capture kinds that trigger notifications")
	fs.StringVar(&cfg.S3.Bucket, "s3-bucket", "", "upload captures to this bucket (S3, GCS interop, MinIO)")
	fs.StringVar(&cfg.S3.Prefix, "s3-prefix", "lastwords", "key prefix in the bucket")
	fs.StringVar(&cfg.S3.Region, "s3-region", "", "bucket region")
	fs.StringVar(&cfg.S3.Endpoint, "s3-endpoint", "", "S3 endpoint (default s3.amazonaws.com)")
	fs.BoolVar(&cfg.S3.Insecure, "s3-insecure", false, "use http for the S3 endpoint")
	namespaces := fs.String("namespaces", "", "only watch these namespaces (comma separated)")
	ignore := fs.String("ignore-namespaces", strings.Join(cfg.IgnoreNamespaces, ","), "never watch these namespaces")
	logLevel := fs.String("log-level", "info", "debug, info, warn, error")
	_ = fs.Parse(args)
	cfg.APIToken = os.Getenv("LASTWORDS_API_TOKEN")
	cfg.NotifyKinds = splitList(*notify)
	cfg.Namespaces = splitList(*namespaces)
	cfg.IgnoreNamespaces = splitList(*ignore)

	var lvl slog.Level
	_ = lvl.UnmarshalText([]byte(*logLevel))
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
	if cfg.NodeName == "" {
		return fmt.Errorf("--node-name (or NODE_NAME) is required")
	}
	procfs.Root = cfg.ProcDir
	jvm.ProcRoot = cfg.ProcDir
	logs.Root = cfg.LogDir

	rc, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("in-cluster config: %w", err)
	}
	kube, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return err
	}
	a, err := agent.New(cfg, kube, log)
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	log.Info("lastwords agent starting", "version", agent.Version, "node", cfg.NodeName, "threshold", cfg.Threshold, "coreDumps", cfg.CoreDumps)
	return a.Run(ctx)
}

func splitList(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

// clientFlags adds the shared cluster flags and returns a constructor.
func clientFlags(fs *flag.FlagSet) func() (*client.Client, error) {
	o := client.DefaultOptions()
	fs.StringVar(&o.Kubeconfig, "kubeconfig", "", "path to kubeconfig")
	fs.StringVar(&o.Context, "context", "", "kubeconfig context")
	fs.StringVar(&o.Namespace, "lastwords-namespace", o.Namespace, "namespace where lastwords is installed")
	return func() (*client.Client, error) { return client.New(o) }
}

// parse lets flags appear before or after positional arguments.
func parse(fs *flag.FlagSet, args []string) []string {
	var pos []string
	for {
		_ = fs.Parse(args)
		args = fs.Args()
		if len(args) == 0 {
			return pos
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func runList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	mk := clientFlags(fs)
	var p client.ListParams
	fs.StringVar(&p.Namespace, "n", "", "namespace")
	fs.StringVar(&p.Namespace, "namespace", "", "namespace")
	fs.StringVar(&p.Pod, "pod", "", "pod name or prefix")
	fs.StringVar(&p.Container, "container", "", "container name")
	fs.StringVar(&p.Kind, "kind", "", "pressure, oomkill, process-oomkill, crash, coredump, evicted")
	fs.StringVar(&p.Since, "since", "7d", "how far back (2h, 3d, or RFC3339)")
	fs.IntVar(&p.Limit, "limit", 50, "maximum rows")
	out := fs.String("o", "table", "table or json")
	pos := parse(fs, args)
	if len(pos) > 0 && p.Pod == "" {
		p.Pod = pos[0]
	}
	c, err := mk()
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	list, err := c.List(ctx, p)
	if *out == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(list)
		return err
	}
	if len(list) == 0 && err == nil {
		fmt.Println("No captures. lastwords records OOM kills, memory pressure, crashes, core dumps and evictions from the moment it is installed.")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tAGE\tKIND\tNAMESPACE\tPOD\tCONTAINER\tWHAT HAPPENED")
	for _, cp := range list {
		h := cp.Headline
		if len(h) > 110 {
			h = h[:107] + "..."
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", cp.ID, age(cp.Time), cp.Kind, cp.Namespace, cp.Pod, cp.Container, h)
	}
	tw.Flush()
	if err != nil {
		fmt.Fprintln(os.Stderr, "\nwarning:", err)
	}
	return nil
}

func age(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func runShow(args []string) error {
	fs := flag.NewFlagSet("show", flag.ExitOnError)
	mk := clientFlags(fs)
	asJSON := fs.Bool("json", false, "print the capture manifest as JSON")
	pos := parse(fs, args)
	if len(pos) != 1 {
		return fmt.Errorf("usage: lastwords show <capture-id>")
	}
	c, err := mk()
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	if *asJSON {
		cp, err := c.Get(ctx, pos[0])
		if err != nil {
			return err
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(cp)
	}
	b, err := c.File(ctx, pos[0], "summary.md", 0, false)
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(b)
	return err
}

func runGet(args []string) error {
	fs := flag.NewFlagSet("get", flag.ExitOnError)
	mk := clientFlags(fs)
	outPath := fs.String("o", "", "output path (default: the file name; - for stdout)")
	pos := parse(fs, args)
	if len(pos) != 2 {
		return fmt.Errorf("usage: lastwords get <capture-id> <file> [-o path]")
	}
	c, err := mk()
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	dst := *outPath
	if dst == "" {
		dst = filepath.Base(pos[1])
	}
	if dst == "-" {
		_, err = c.Download(ctx, pos[0], pos[1], os.Stdout)
		return err
	}
	// Write next to the destination and rename, so an interrupted download
	// never leaves a truncated core that looks complete.
	tmp := dst + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	n, err := c.Download(ctx, pos[0], pos[1], f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s (%s)\n", dst, procfs.Bytes(uint64(n)))
	return nil
}

func runTop(args []string) error {
	fs := flag.NewFlagSet("top", flag.ExitOnError)
	mk := clientFlags(fs)
	ns := fs.String("n", "", "namespace")
	n := fs.Int("limit", 20, "rows")
	parse(fs, args)
	c, err := mk()
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	items, err := c.Watchlist(ctx)
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAMESPACE\tPOD\tCONTAINER\tNODE\tWORKING SET\tLIMIT\t%\tGROWTH\tOOM IN")
	rows := 0
	for _, it := range items {
		if (*ns != "" && it.Namespace != *ns) || it.LimitBytes == 0 {
			continue
		}
		if rows == *n {
			break
		}
		rows++
		growth, eta := "-", "-"
		if it.GrowthBytesPerS > 64*1024 {
			growth = procfs.Bytes(uint64(it.GrowthBytesPerS)) + "/s"
			if it.SecondsToLimit > 0 {
				eta = (time.Duration(it.SecondsToLimit) * time.Second).Round(time.Second).String()
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%.0f%%\t%s\t%s\n", it.Namespace, it.Pod, it.Container, it.Node, procfs.Bytes(it.WorkingSetBytes), procfs.Bytes(it.LimitBytes), it.Ratio*100, growth, eta)
	}
	tw.Flush()
	if err != nil {
		fmt.Fprintln(os.Stderr, "\nwarning:", err)
	}
	return nil
}

func runMCP(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	mk := clientFlags(fs)
	addr := fs.String("http", "", "serve MCP over streamable HTTP on this address (e.g. :8090) instead of stdio; needs LASTWORDS_MCP_TOKEN")
	parse(fs, args)
	ctx, cancel := signalContext()
	defer cancel()
	if *addr != "" {
		return mcpserver.RunHTTP(ctx, *addr, os.Getenv("LASTWORDS_MCP_TOKEN"), mk, agent.Version)
	}
	return mcpserver.Run(ctx, mk, agent.Version)
}
