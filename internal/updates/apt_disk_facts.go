package updates

import (
	"math/big"
	"regexp"
	"strings"
)

type aptDiskFacts struct {
	ArchiveBytes        int64
	InstalledDeltaBytes int64
}

var aptByteSizePattern = regexp.MustCompile(`^([0-9]+|[0-9]{1,3}(?:,[0-9]{3})+)(?:\.([0-9]+))?[[:space:]]+(B|kB|KB|MB|GB|KiB|MiB|GiB)$`)

func parseAptByteSize(raw string) (int64, bool) {
	matches := aptByteSizePattern.FindStringSubmatch(strings.TrimSpace(raw))
	if len(matches) != 4 {
		return 0, false
	}
	multiplier, ok := aptByteUnitMultiplier(matches[3])
	if !ok {
		return 0, false
	}
	whole := new(big.Int)
	if _, ok := whole.SetString(strings.ReplaceAll(matches[1], ",", ""), 10); !ok {
		return 0, false
	}
	value := new(big.Int).Mul(whole, big.NewInt(multiplier))
	if fraction := matches[2]; fraction != "" {
		fractionValue := new(big.Int)
		if _, ok := fractionValue.SetString(fraction, 10); !ok {
			return 0, false
		}
		denominator := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(len(fraction))), nil)
		fractionBytes := new(big.Int).Mul(fractionValue, big.NewInt(multiplier))
		fractionBytes.Add(fractionBytes, new(big.Int).Sub(denominator, big.NewInt(1)))
		fractionBytes.Quo(fractionBytes, denominator)
		value.Add(value, fractionBytes)
	}
	if !value.IsInt64() {
		return 0, false
	}
	return value.Int64(), true
}

func aptByteUnitMultiplier(unit string) (int64, bool) {
	switch unit {
	case "B":
		return 1, true
	case "kB", "KB":
		return 1000, true
	case "MB":
		return 1000 * 1000, true
	case "GB":
		return 1000 * 1000 * 1000, true
	case "KiB":
		return 1024, true
	case "MiB":
		return 1024 * 1024, true
	case "GiB":
		return 1024 * 1024 * 1024, true
	default:
		return 0, false
	}
}

func parseAptDiskFacts(output string) (aptDiskFacts, bool) {
	var facts aptDiskFacts
	archiveSeen := false
	deltaSeen := false
	for _, rawLine := range strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(rawLine)
		switch {
		case strings.HasPrefix(line, "Need to get ") && strings.HasSuffix(strings.TrimSuffix(line, "."), " of archives"):
			body := strings.TrimSuffix(strings.TrimPrefix(line, "Need to get "), ".")
			body = strings.TrimSuffix(body, " of archives")
			if slash := strings.Index(body, "/"); slash >= 0 {
				body = strings.TrimSpace(body[:slash])
			}
			value, ok := parseAptByteSize(body)
			if !ok || archiveSeen {
				return aptDiskFacts{}, false
			}
			facts.ArchiveBytes = value
			archiveSeen = true
		case strings.HasPrefix(line, "After this operation, ") && strings.HasSuffix(strings.TrimSuffix(line, "."), " of additional disk space will be used"):
			body := strings.TrimSuffix(strings.TrimPrefix(line, "After this operation, "), ".")
			body = strings.TrimSuffix(body, " of additional disk space will be used")
			value, ok := parseAptByteSize(body)
			if !ok || deltaSeen {
				return aptDiskFacts{}, false
			}
			facts.InstalledDeltaBytes = value
			deltaSeen = true
		case strings.HasPrefix(line, "After this operation, ") && strings.HasSuffix(strings.TrimSuffix(line, "."), " disk space will be freed"):
			body := strings.TrimSuffix(strings.TrimPrefix(line, "After this operation, "), ".")
			body = strings.TrimSuffix(body, " disk space will be freed")
			value, ok := parseAptByteSize(body)
			if !ok || deltaSeen {
				return aptDiskFacts{}, false
			}
			facts.InstalledDeltaBytes = -value
			deltaSeen = true
		}
	}
	return facts, archiveSeen && deltaSeen
}
