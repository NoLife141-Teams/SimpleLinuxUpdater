package updates

import "strings"

// Debian Policy 5.6.12 / dpkg verrevcmp ordering. Compare digit runs by length
// after leading-zero removal (no integer overflow); '~' precedes even EOF.
// https://www.debian.org/doc/debian-policy/ch-controlfields.html#version
func compareDebianVersions(a, b string) (int, bool) {
	ae, au, ar, ok := splitDebianVersion(a)
	if !ok {
		return 0, false
	}
	be, bu, br, ok := splitDebianVersion(b)
	if !ok {
		return 0, false
	}
	if c := compareDebianDigits(ae, be); c != 0 {
		return c, true
	}
	if c := compareDebianPart(au, bu); c != 0 {
		return c, true
	}
	return compareDebianPart(ar, br), true
}
func splitDebianVersion(v string) (epoch, upstream, revision string, ok bool) {
	if v == "" || len(v) > 512 {
		return
	}
	epoch, revision = "0", "0"
	if i := strings.IndexByte(v, ':'); i >= 0 {
		epoch, v = v[:i], v[i+1:]
		if epoch == "" {
			return
		}
		for _, c := range epoch {
			if c < '0' || c > '9' {
				return
			}
		}
	}
	if i := strings.LastIndexByte(v, '-'); i >= 0 {
		v, revision = v[:i], v[i+1:]
		if revision == "" {
			return
		}
	}
	if v == "" || v[0] < '0' || v[0] > '9' {
		return
	}
	for _, c := range v {
		if !(c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || strings.ContainsRune(".+:~-", c)) {
			return
		}
	}
	for _, c := range revision {
		if !(c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || strings.ContainsRune(".+~", c)) {
			return
		}
	}
	return epoch, v, revision, true
}
func compareDebianDigits(a, b string) int {
	a, b = strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")
	if len(a) < len(b) {
		return -1
	}
	if len(a) > len(b) {
		return 1
	}
	return strings.Compare(a, b)
}
func debianOrder(c byte) int {
	if c == '~' {
		return -1
	}
	if c == 0 || c >= '0' && c <= '9' {
		return 0
	}
	if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' {
		return int(c)
	}
	return int(c) + 256
}
func compareDebianPart(a, b string) int {
	for len(a) > 0 || len(b) > 0 {
		for len(a) > 0 && (a[0] < '0' || a[0] > '9') || len(b) > 0 && (b[0] < '0' || b[0] > '9') {
			var ac, bc byte
			if len(a) > 0 {
				ac = a[0]
			}
			if len(b) > 0 {
				bc = b[0]
			}
			if d := debianOrder(ac) - debianOrder(bc); d < 0 {
				return -1
			} else if d > 0 {
				return 1
			}
			if len(a) > 0 {
				a = a[1:]
			}
			if len(b) > 0 {
				b = b[1:]
			}
		}
		ai, bi := 0, 0
		for ai < len(a) && a[ai] >= '0' && a[ai] <= '9' {
			ai++
		}
		for bi < len(b) && b[bi] >= '0' && b[bi] <= '9' {
			bi++
		}
		if c := compareDebianDigits(a[:ai], b[:bi]); c != 0 {
			return c
		}
		a, b = a[ai:], b[bi:]
	}
	return 0
}
