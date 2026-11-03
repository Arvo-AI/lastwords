// Package agent is the per-node lastwords daemon: it watches container memory,
// captures evidence before OOM kills, and records crashes, core dumps and
// evictions.
package agent

import "time"

// Config holds agent settings (flags / Helm values).
type Config struct {
	NodeName  string
	Cluster   string
	DataDir   string // as seen by the agent, e.g. /var/lib/lastwords
	CgroupDir string // host cgroup mount, e.g. /host/sys/fs/cgroup
	ProcDir   string // host /proc (with hostPID this is /proc)
	LogDir    string // host /var/log/pods

	Interval    time.Duration // sampling interval
	Threshold   float64       // working set / limit that triggers a capture
	LastGasp    float64       // second, later capture tier
	Predictive  bool          // capture early if the limit will be hit within PredictWindow
	PredictMin  float64       // only predict above this ratio
	PredictSecs float64       // seconds-to-limit that counts as imminent
	// JVMGCHeadroom is the share of the limit kept free for a JVM's next GC.
	// A young collection copies live objects into the old generation, so a
	// JVM whose -Xmx exceeds its limit is killed at roughly 1 - headroom of
	// the limit, not at 100%. Capture tiers for JVMs are measured against
	// that lower point.
	JVMGCHeadroom float64
	History       time.Duration // memory timeline kept per container

	CaptureTimeout   time.Duration
	MaxConcurrent    int
	JVMHistogram     bool
	JVMThreadDump    bool
	JVMNativeMemory  bool
	AllowHeapDumps   bool  // cluster-wide default for full heap dumps (pods can opt in by annotation)
	HeapDumpMaxBytes int64 // heap dumps larger than this (gzipped) are dropped
	GoPprofPort      int   // default pprof port to try for Go programs
	CaptureLogs      bool
	LogLines         int

	CoreDumps         bool
	CoreHostDir       string // the data dir as the host kernel sees it
	CoreMaxBytes      int64
	RestoreCoreOnExit bool

	MaxStoreBytes int64
	MaxAge        time.Duration

	ListenAddr string
	APIToken   string

	WebhookURL      string
	SlackWebhookURL string
	NotifyKinds     []string

	S3 S3Config

	Namespaces       []string // only watch these namespaces (empty = all)
	IgnoreNamespaces []string
}

// S3Config configures optional upload of captures to object storage.
type S3Config struct {
	Bucket   string
	Prefix   string
	Region   string
	Endpoint string // s3.amazonaws.com, storage.googleapis.com, minio:9000
	Insecure bool
}

// Defaults returns sensible defaults.
func Defaults() Config {
	return Config{
		DataDir:           "/var/lib/lastwords",
		CgroupDir:         "/host/sys/fs/cgroup",
		ProcDir:           "/proc",
		LogDir:            "/var/log/pods",
		Interval:          500 * time.Millisecond,
		Threshold:         0.90,
		LastGasp:          0.97,
		Predictive:        true,
		PredictMin:        0.70,
		PredictSecs:       15,
		JVMGCHeadroom:     0.25,
		History:           10 * time.Minute,
		CaptureTimeout:    60 * time.Second,
		MaxConcurrent:     2,
		JVMHistogram:      true,
		JVMThreadDump:     true,
		JVMNativeMemory:   true,
		HeapDumpMaxBytes:  2 << 30,
		GoPprofPort:       6060,
		CaptureLogs:       true,
		LogLines:          500,
		CoreMaxBytes:      2 << 30,
		RestoreCoreOnExit: true,
		MaxStoreBytes:     5 << 30,
		MaxAge:            7 * 24 * time.Hour,
		ListenAddr:        ":9095",
		NotifyKinds:       []string{"pressure", "oomkill", "process-oomkill", "crash", "coredump", "evicted"},
		IgnoreNamespaces:  []string{"kube-system"},
	}
}
