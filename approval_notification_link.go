package main

import (
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	serverpkg "debian-updater/internal/servers"
)

const applicationURLEnv = "DEBIAN_UPDATER_PUBLIC_URL"

var approvalLinkID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)
var dottedIPv4Origin = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+\.?$`)

type approvalLink struct {
	ServerIdentity string
	JobID          string
	Generation     uint64
}

func parseApplicationOrigin(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	origin, err := url.Parse(raw)
	if err != nil || (origin.Scheme != "https" && origin.Scheme != "http") ||
		!validApplicationOriginHost(origin.Hostname()) || origin.User != nil || origin.Opaque != "" ||
		(origin.EscapedPath() != "" && origin.EscapedPath() != "/") || strings.ContainsAny(raw, "?#") {
		return nil, errors.New("use an absolute HTTP(S) origin without credentials, a path, a query, or a fragment")
	}
	if port := origin.Port(); port != "" || strings.HasSuffix(origin.Host, ":") {
		number, err := strconv.ParseUint(port, 10, 16)
		if err != nil || number == 0 {
			return nil, errors.New("the public URL port must be between 1 and 65535")
		}
	}
	origin.Path = "/"
	return origin, nil
}

func validApplicationOriginHost(host string) bool {
	if host == "" || strings.ContainsRune(host, '%') || strings.ContainsFunc(host, unicode.IsSpace) {
		return false
	}
	if dottedIPv4Origin.MatchString(host) {
		// net/url accepts an out-of-range dotted IPv4 token as a DNS name.
		// The browser treats it as an address and rejects invalid octets.
		for _, octet := range strings.Split(strings.TrimSuffix(host, "."), ".") {
			if _, err := strconv.ParseUint(octet, 0, 8); err != nil {
				return false
			}
		}
	}
	return true
}

func (link approvalLink) valid() bool {
	return approvalLinkID.MatchString(link.ServerIdentity) && approvalLinkID.MatchString(link.JobID) && link.Generation > 0
}

func (link approvalLink) url(origin url.URL) string {
	origin.Path = "/"
	origin.RawQuery = url.Values{
		"server_identity":     {link.ServerIdentity},
		"job_id":              {link.JobID},
		"approval_generation": {strconv.FormatUint(link.Generation, 10)},
	}.Encode()
	return origin.String()
}

func parseApprovalLink(query url.Values) (approvalLink, bool) {
	generation, err := strconv.ParseUint(query.Get("approval_generation"), 10, 64)
	link := approvalLink{ServerIdentity: query.Get("server_identity"), JobID: query.Get("job_id"), Generation: generation}
	return link, err == nil && link.valid() && len(query) == 3 &&
		len(query["server_identity"]) == 1 && len(query["job_id"]) == 1 && len(query["approval_generation"]) == 1
}

func approvalReturnDestination(raw string) string {
	// Rebuild the destination from the only three supported parameters.
	target, err := url.ParseRequestURI(raw)
	if err != nil || target.IsAbs() || target.Host != "" || target.EscapedPath() != "/" {
		return "/"
	}
	query, err := url.ParseQuery(target.RawQuery)
	link, valid := parseApprovalLink(query)
	if err != nil || !valid {
		return "/"
	}
	return link.url(url.URL{})
}

func authPageDestination(path, returnTo string) string {
	if returnTo == "/" {
		return path
	}
	return path + "?" + url.Values{"return_to": {returnTo}}.Encode()
}

type approvalAuthView struct {
	ReturnTo  string
	LoginURL  string
	SetupURL  string
	SubmitURL string
}

func newApprovalAuthView(raw, submitPath string) approvalAuthView {
	returnTo := approvalReturnDestination(raw)
	return approvalAuthView{
		ReturnTo:  returnTo,
		LoginURL:  authPageDestination("/login", returnTo),
		SetupURL:  authPageDestination("/setup", returnTo),
		SubmitURL: authPageDestination(submitPath, returnTo),
	}
}

func approvalLinkProblem(state *serverpkg.State, link approvalLink) string {
	state.Lock()
	defer state.Unlock()
	for _, server := range state.Servers() {
		if server.Identity != link.ServerIdentity {
			continue
		}
		status := state.StatusMap()[server.Name]
		switch {
		case status == nil || status.Status != "pending_approval" || status.JobID != link.JobID:
			return "This approval has expired or is no longer pending."
		case status.ApprovalGeneration != link.Generation:
			return "This approval plan was replaced. Select the host in Status to review the current plan."
		default:
			return ""
		}
	}
	return "The linked host was removed or replaced in the inventory."
}
