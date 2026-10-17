// Package health samples Pi health metrics for the 15 s stat message:
// vcgencmd get_throttled / measure_temp and WiFi RSSI from
// /proc/net/wireless. Every probe fails soft — on a dev machine (no
// vcgencmd, no /proc/net/wireless) the fields come back zero and the agent
// keeps running.
package health

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Sample is one health snapshot. Missing metrics stay at their zero value.
type Sample struct {
	TempC     float64
	Throttled string // raw hex string from vcgencmd, e.g. "0x0"; "" = unknown
	WifiRSSI  int    // dBm; 0 = unknown
}

var (
	vcgencmdOnce sync.Once
	vcgencmdPath string
)

func vcgencmd() string {
	vcgencmdOnce.Do(func() {
		vcgencmdPath, _ = exec.LookPath("vcgencmd")
	})
	return vcgencmdPath
}

// Read collects one sample. Never fails: every probe is optional.
func Read(ctx context.Context) Sample {
	var s Sample
	if bin := vcgencmd(); bin != "" {
		if out, err := run(ctx, bin, "get_throttled"); err == nil {
			// "throttled=0x50005" — keep the hex string verbatim for the hub.
			if v, ok := strings.CutPrefix(strings.TrimSpace(out), "throttled="); ok {
				s.Throttled = v
			}
		}
		if out, err := run(ctx, bin, "measure_temp"); err == nil {
			// "temp=51.2'C"
			v := strings.TrimSpace(out)
			v = strings.TrimPrefix(v, "temp=")
			v = strings.TrimSuffix(v, "'C")
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				s.TempC = f
			}
		}
	}
	s.WifiRSSI = wifiRSSI()
	return s
}

func run(ctx context.Context, name string, args ...string) (string, error) {
	c, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(c, name, args...).Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// wifiRSSI parses /proc/net/wireless:
//
//	Inter-| sta-|   Quality        |   Discarded packets               | Missed | WE
//	 face | tus | link level noise |  nwid  crypt   frag  retry   misc | beacon | 22
//	 wlan0: 0000   67.  -58.  -256        0      0      0      0      0      0
//
// Level is column index 3, suffixed with '.'. Absent on macOS/eth → 0.
func wifiRSSI() int {
	data, err := os.ReadFile("/proc/net/wireless")
	if err != nil {
		return 0
	}
	for _, line := range bytes.Split(data, []byte("\n")) {
		fields := strings.Fields(string(line))
		// Data rows start with "<iface>:" and carry 10 fields.
		if len(fields) < 4 || !strings.HasSuffix(fields[0], ":") {
			continue
		}
		v := strings.TrimSuffix(fields[3], ".")
		if rssi, err := strconv.Atoi(v); err == nil {
			return rssi
		}
	}
	return 0
}
