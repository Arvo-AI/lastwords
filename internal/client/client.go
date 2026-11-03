// Package client queries every lastwords agent in a cluster through the
// Kubernetes API server's pod proxy, so it needs nothing but a kubeconfig.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/Arvo-AI/lastwords/internal/store"
)

// Options select the cluster and the agent install.
type Options struct {
	Kubeconfig string
	Context    string
	Namespace  string // where lastwords is installed
	Selector   string
	Port       string
}

// DefaultOptions match the Helm chart.
func DefaultOptions() Options {
	return Options{Namespace: "lastwords", Selector: "app.kubernetes.io/name=lastwords", Port: "9095"}
}

// Client talks to all agents.
type Client struct {
	opts   Options
	kube   kubernetes.Interface
	stream kubernetes.Interface // no overall timeout, for large downloads

	tokenOnce sync.Once
	token     string
	tokenErr  error
}

// AgentPod is one agent instance.
type AgentPod struct {
	Name string
	Node string
}

// New builds a client from a kubeconfig.
func New(opts Options) (*Client, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if opts.Kubeconfig != "" {
		rules.ExplicitPath = opts.Kubeconfig
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: opts.Context}).ClientConfig()
	if err != nil {
		// Fall back to in-cluster config (e.g. running inside Aurora or a CI pod).
		if ic, ierr := rest.InClusterConfig(); ierr == nil {
			cfg = ic
		} else {
			return nil, fmt.Errorf("load kubeconfig: %w", err)
		}
	}
	stream, err := kubernetes.NewForConfig(rest.CopyConfig(cfg)) // bounded by the caller's context only
	if err != nil {
		return nil, err
	}
	cfg.Timeout = 60 * time.Second
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Client{opts: opts, kube: kube, stream: stream}, nil
}

// apiToken reads the agents' shared token from the lastwords-api Secret once.
func (c *Client) apiToken(ctx context.Context) (string, error) {
	c.tokenOnce.Do(func() {
		s, err := c.kube.CoreV1().Secrets(c.opts.Namespace).Get(ctx, "lastwords-api", metav1.GetOptions{})
		switch {
		case apierrors.IsForbidden(err):
			c.tokenErr = fmt.Errorf("not allowed to read secret %s/lastwords-api, which holds the agent API token (bind the lastwords-reader Role): %w", c.opts.Namespace, err)
		case err != nil:
			c.tokenErr = fmt.Errorf("read secret %s/lastwords-api: %w", c.opts.Namespace, err)
		case len(s.Data["token"]) == 0:
			c.tokenErr = fmt.Errorf("secret %s/lastwords-api has no token", c.opts.Namespace)
		default:
			c.token = string(s.Data["token"])
		}
	})
	return c.token, c.tokenErr
}

// Agents lists running agent pods.
func (c *Client) Agents(ctx context.Context) ([]AgentPod, error) {
	pods, err := c.kube.CoreV1().Pods(c.opts.Namespace).List(ctx, metav1.ListOptions{LabelSelector: c.opts.Selector})
	if err != nil {
		if apierrors.IsForbidden(err) {
			return nil, fmt.Errorf("not allowed to list pods in namespace %q: %w", c.opts.Namespace, err)
		}
		return nil, err
	}
	var out []AgentPod
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodRunning {
			out = append(out, AgentPod{Name: p.Name, Node: p.Spec.NodeName})
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no running lastwords agents in namespace %q (selector %s). Install with: helm install lastwords oci://ghcr.io/arvo-ai/charts/lastwords -n lastwords --create-namespace", c.opts.Namespace, c.opts.Selector)
	}
	return out, nil
}

func (c *Client) request(ctx context.Context, kube kubernetes.Interface, pod, path string, params map[string]string) (*rest.Request, error) {
	token, err := c.apiToken(ctx)
	if err != nil {
		return nil, err
	}
	req := kube.CoreV1().RESTClient().Get().
		Namespace(c.opts.Namespace).Resource("pods").Name(pod+":"+c.opts.Port).SubResource("proxy").Suffix(path).
		SetHeader("X-Lastwords-Token", token)
	for k, v := range params {
		if v != "" {
			req = req.Param(k, v)
		}
	}
	return req, nil
}

func (c *Client) get(ctx context.Context, pod, path string, params map[string]string) ([]byte, error) {
	req, err := c.request(ctx, c.kube, pod, path, params)
	if err != nil {
		return nil, err
	}
	return req.DoRaw(ctx)
}

// ListParams filter captures.
type ListParams struct {
	Namespace string
	Pod       string
	Container string
	Kind      string
	Since     string
	Limit     int
}

// List queries all agents in parallel and merges results, newest first.
// Agents that fail are reported in the error but do not hide others' results.
func (c *Client) List(ctx context.Context, p ListParams) ([]*store.Capture, error) {
	agents, err := c.Agents(ctx)
	if err != nil {
		return nil, err
	}
	params := map[string]string{"namespace": p.Namespace, "pod": p.Pod, "container": p.Container, "kind": p.Kind, "since": p.Since}
	if p.Limit > 0 {
		params["limit"] = strconv.Itoa(p.Limit)
	}
	var mu sync.Mutex
	var all []*store.Capture
	var errs []error
	var wg sync.WaitGroup
	for _, a := range agents {
		wg.Add(1)
		go func(a AgentPod) {
			defer wg.Done()
			b, err := c.get(ctx, a.Name, "/v1/captures", params)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, fmt.Errorf("agent %s (node %s): %w", a.Name, a.Node, err))
				return
			}
			var list []*store.Capture
			if err := json.Unmarshal(b, &list); err != nil {
				errs = append(errs, fmt.Errorf("agent %s: %w", a.Name, err))
				return
			}
			all = append(all, list...)
		}(a)
	}
	wg.Wait()
	sort.Slice(all, func(i, j int) bool { return all[i].Time.After(all[j].Time) })
	if p.Limit > 0 && len(all) > p.Limit {
		all = all[:p.Limit]
	}
	return all, errors.Join(errs...)
}

// find locates which agent holds a capture. Capture IDs are unique, so we ask
// every agent and take the one that answers.
func (c *Client) find(ctx context.Context, id string) (string, *store.Capture, error) {
	agents, err := c.Agents(ctx)
	if err != nil {
		return "", nil, err
	}
	type res struct {
		pod string
		c   *store.Capture
	}
	ch := make(chan res, len(agents))
	for _, a := range agents {
		go func(a AgentPod) {
			b, err := c.get(ctx, a.Name, "/v1/captures/"+url.PathEscape(id), nil)
			if err != nil {
				ch <- res{}
				return
			}
			var cp store.Capture
			if json.Unmarshal(b, &cp) != nil {
				ch <- res{}
				return
			}
			ch <- res{a.Name, &cp}
		}(a)
	}
	for range agents {
		if r := <-ch; r.c != nil {
			return r.pod, r.c, nil
		}
	}
	return "", nil, fmt.Errorf("capture %s not found on any agent (it may have been pruned, or its node is gone)", id)
}

// Get returns a capture manifest.
func (c *Client) Get(ctx context.Context, id string) (*store.Capture, error) {
	_, cp, err := c.find(ctx, id)
	return cp, err
}

// File returns a capture file. max > 0 limits the bytes (tail=true takes the end).
func (c *Client) File(ctx context.Context, id, name string, max int64, tail bool) ([]byte, error) {
	pod, _, err := c.find(ctx, id)
	if err != nil {
		return nil, err
	}
	params := map[string]string{}
	if max > 0 {
		params["max"] = strconv.FormatInt(max, 10)
		if tail {
			params["tail"] = "1"
		}
	}
	return c.get(ctx, pod, "/v1/captures/"+url.PathEscape(id)+"/files/"+url.PathEscape(name), params)
}

// Download streams a whole capture file (cores and heap dumps can be GBs) to w
// and returns the bytes written.
func (c *Client) Download(ctx context.Context, id, name string, w io.Writer) (int64, error) {
	pod, _, err := c.find(ctx, id)
	if err != nil {
		return 0, err
	}
	req, err := c.request(ctx, c.stream, pod, "/v1/captures/"+url.PathEscape(id)+"/files/"+url.PathEscape(name), nil)
	if err != nil {
		return 0, err
	}
	body, err := req.Stream(ctx)
	if err != nil {
		return 0, err
	}
	defer body.Close()
	return io.Copy(w, body)
}

// WatchItem mirrors agent.WatchItem without importing the agent.
type WatchItem struct {
	Node            string  `json:"node"`
	Namespace       string  `json:"namespace"`
	Pod             string  `json:"pod"`
	Container       string  `json:"container"`
	WorkingSetBytes uint64  `json:"workingSetBytes"`
	LimitBytes      uint64  `json:"limitBytes"`
	Ratio           float64 `json:"ratio"`
	GrowthBytesPerS float64 `json:"growthBytesPerSecond"`
	SecondsToLimit  float64 `json:"secondsToLimit,omitempty"`
	OOMKills        uint64  `json:"oomKills"`
}

// Watchlist returns live memory state for all containers, highest ratio first.
func (c *Client) Watchlist(ctx context.Context) ([]WatchItem, error) {
	agents, err := c.Agents(ctx)
	if err != nil {
		return nil, err
	}
	var mu sync.Mutex
	var all []WatchItem
	var errs []error
	var wg sync.WaitGroup
	for _, a := range agents {
		wg.Add(1)
		go func(a AgentPod) {
			defer wg.Done()
			b, err := c.get(ctx, a.Name, "/v1/watchlist", nil)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, fmt.Errorf("agent %s: %w", a.Name, err))
				return
			}
			var items []WatchItem
			if err := json.Unmarshal(b, &items); err == nil {
				for i := range items {
					items[i].Node = a.Node
				}
				all = append(all, items...)
			}
		}(a)
	}
	wg.Wait()
	sort.Slice(all, func(i, j int) bool { return all[i].Ratio > all[j].Ratio })
	return all, errors.Join(errs...)
}
