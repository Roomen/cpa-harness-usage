// Command fakeccusage is a synthetic test double for the pinned ccusage CLI.
// It never reads real harness logs; all output comes from environment values.
package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		os.Exit(2)
	}
	source := os.Args[1]
	prefix := "FAKE_" + strings.ToUpper(source) + "_"
	mode := env(prefix + "MODE")
	switch mode {
	case "fail":
		// Stderr must never reach the report.
		fmt.Fprintln(os.Stderr, "synthetic diagnostic line that must never reach the report")
		os.Exit(1)
	case "sleep":
		time.Sleep(duration(env(prefix + "SLEEP")))
	case "large":
		fmt.Print(strings.Repeat("x", 4096))
		return
	case "path":
		// Succeed only when the adapter received the expected store path.
		want := env(prefix + "PI_PATH")
		matched := want != ""
		if matched {
			matched = false
			for index, argument := range os.Args {
				if argument == "--pi-path" && index+1 < len(os.Args) && os.Args[index+1] == want {
					matched = true
				}
			}
		}
		if !matched {
			os.Exit(3)
		}
	case "ok", "empty", "badjson", "unknown", "":
	default:
		os.Exit(2)
	}
	if mode == "badjson" {
		fmt.Print("not json")
		return
	}
	if mode == "unknown" {
		fmt.Print(`{"rows":[],"totals":{}}`)
		return
	}
	fmt.Print(env(prefix + "BODY"))
}

func env(name string) string {
	return os.Getenv(name)
}

func duration(value string) time.Duration {
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return time.Second
	}
	return parsed
}
