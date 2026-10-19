package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"cpa-harness-usage/internal/usage"
)

func main() {
	binary := flag.String("binary", "ccusage", "path to the ccusage executable (default: ccusage on PATH)")
	sources := flag.String("sources", "claude,codex,opencode", "comma-separated sources: claude,codex,opencode")
	timeout := flag.Duration("timeout", usage.DefaultCommandTimeout, "per-source scan timeout")
	flag.Parse()

	list := make([]string, 0, 3)
	for _, item := range strings.Split(*sources, ",") {
		if item = strings.TrimSpace(item); item != "" {
			list = append(list, item)
		}
	}
	if len(list) == 0 {
		fmt.Fprintln(os.Stderr, "no sources selected")
		os.Exit(2)
	}
	if *timeout <= 0 {
		fmt.Fprintln(os.Stderr, "timeout must be positive")
		os.Exit(2)
	}

	collector := usage.New(usage.Config{Binary: *binary, Sources: list, Timeout: *timeout})
	report := collector.Snapshot(context.Background())

	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		fmt.Fprintln(os.Stderr, "could not encode usage report")
		os.Exit(1)
	}
	// Exit status stays zero when individual sources fail: the report is a
	// valid partial result and callers should inspect sources[].status.
}
