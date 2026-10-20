//go:build bundledcollector

package usage

import "embed"

//go:embed collectorassets/*
var collectorAssets embed.FS

func embeddedCollector() ([]byte, []byte, error) {
	manifest, err := collectorAssets.ReadFile("collectorassets/manifest.json")
	if err != nil {
		return nil, nil, err
	}
	payload, err := collectorAssets.ReadFile("collectorassets/ccusage.bin")
	return payload, manifest, err
}
