//go:build !bundledcollector

package usage

func embeddedCollector() ([]byte, []byte, error) {
	return nil, nil, errCollectorUnavailable
}
