// Command compare drives the same client-visible requests against the legacy
// TypeScript server and the Go rewrite, then reports every wire divergence.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

type options struct {
	mode       string
	scenarios  map[string]bool
	keep       bool
	large      bool
	tsRepo     string
	adminURL   string
	goPort     int
	tsPort     int
	healthWait time.Duration
}

func main() {
	var scenarioList string
	opts := options{}
	flag.StringVar(&opts.mode, "mode", "all", "auth mode to compare: dev, password, or all")
	flag.StringVar(&scenarioList, "scenario", "", "comma-separated scenario names (default: all)")
	flag.BoolVar(&opts.keep, "keep", false, "keep scratch databases and blob directories")
	flag.BoolVar(&opts.large, "large", true, "run the 100 MiB and batch-cap boundary checks")
	flag.StringVar(&opts.tsRepo, "ts-repo", "/home/justin/Developer/futo-notes-server", "legacy TypeScript server checkout")
	flag.StringVar(&opts.adminURL, "postgres", "postgres://futo_notes:futo_notes@localhost:5433/futo_notes", "Postgres administration URL")
	flag.IntVar(&opts.goPort, "go-port", 3005, "Go server port")
	flag.IntVar(&opts.tsPort, "ts-port", 3105, "TypeScript server port")
	flag.DurationVar(&opts.healthWait, "health-timeout", 30*time.Second, "server startup timeout")
	flag.Parse()

	if opts.mode != "dev" && opts.mode != "password" && opts.mode != "all" {
		fmt.Fprintln(os.Stderr, "-mode must be dev, password, or all")
		os.Exit(2)
	}
	opts.scenarios = map[string]bool{}
	for _, name := range strings.Split(scenarioList, ",") {
		if name = strings.TrimSpace(name); name != "" {
			opts.scenarios[name] = true
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := time.Now()
	result := &runResult{}
	modes := []runConfig{}
	if opts.mode == "dev" || opts.mode == "all" {
		modes = append(modes, runConfig{name: "dev", authMode: "dev"})
	}
	if opts.mode == "password" || opts.mode == "all" {
		modes = append(modes,
			runConfig{name: "password/plaintext", authMode: "password", password: comparisonPassword},
			runConfig{name: "password/scrypt", authMode: "password", passwordHash: comparisonPasswordHash},
		)
	}

	for _, cfg := range modes {
		fmt.Printf("\n== %s ==\n", cfg.name)
		modeResult, err := runComparison(ctx, opts, cfg)
		result.merge(modeResult)
		if err != nil {
			result.Infrastructure = append(result.Infrastructure, cfg.name+": "+err.Error())
			fmt.Printf("infrastructure failure: %v\n", err)
		}
	}

	fmt.Printf("\n== comparison summary ==\n")
	fmt.Printf("steps: %d, matched: %d, accepted deviations: %d, divergences: %d, duration: %s\n",
		result.Steps, result.Matched, len(result.Accepted), len(result.Divergences), time.Since(started).Round(time.Millisecond))
	for _, item := range result.Accepted {
		fmt.Printf("ALLOW %s\n", item)
	}
	for _, item := range result.Divergences {
		fmt.Printf("DIFF  %s\n", item)
	}
	for _, item := range result.Infrastructure {
		fmt.Printf("ERROR %s\n", item)
	}
	if len(result.Divergences) != 0 || len(result.Infrastructure) != 0 {
		os.Exit(1)
	}
}

const comparisonPassword = "comparison-harness-password"

// Fixed vector for comparisonPassword using the format implemented by both
// servers. Keeping it fixed makes startup deterministic and avoids depending
// on either implementation to generate the other's test input.
const comparisonPasswordHash = "scrypt:N=16384,r=8,p=1:9e0c3a72192b72ef1f311ead3d31f8ffb3f375061f668d7aeb117bd97d5887eb:5e011d207d2ae69d3efc308bef616088ce03a47cba3cf5bf68c6bb74197f2a1fec29c1da6899598434d8c2809949154d30f4f0d5d23fc56a34cba037b6ced5b0"

type runResult struct {
	Steps          int
	Matched        int
	Accepted       []string
	Divergences    []string
	Infrastructure []string
}

func (r *runResult) merge(other runResult) {
	r.Steps += other.Steps
	r.Matched += other.Matched
	r.Accepted = append(r.Accepted, other.Accepted...)
	r.Divergences = append(r.Divergences, other.Divergences...)
	r.Infrastructure = append(r.Infrastructure, other.Infrastructure...)
}
