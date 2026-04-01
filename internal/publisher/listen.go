package publisher

import (
	"strings"
)

const defaultListenHost = "0.0.0.0"

func trimListenHost(host string) string {
	h := strings.TrimSpace(host)
	if h == "" {
		return defaultListenHost
	}
	return h
}
