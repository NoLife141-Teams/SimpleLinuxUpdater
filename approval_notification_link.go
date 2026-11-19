package main

import (
	"encoding/json"
	"errors"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	serverpkg "debian-updater/internal/servers"
	"golang.org/x/net/idna"
)

const applicationURLEnv = "DEBIAN_UPDATER_PUBLIC_URL"

const (
	approvalLinkExpired     = "This approval has expired or is no longer pending."
	approvalLinkReplaced    = "This approval plan was replaced. Select the host in Status to review the current plan."
	approvalLinkMissingHost = "The linked host was removed or replaced in the inventory."
)

var approvalLinkID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)
var applicationOriginIDNA = idna.New(idna.MapForLookup(), idna.StrictDomainName(false), idna.CheckHyphens(false), idna.BidiRule(), idna.VerifyDNSLength(true))
var applicationOriginDNSName = regexp.MustCompile(`^[a-z0-9_-]+(\.[a-z0-9_-]+)*$`)
var browserIPv4Label = regexp.MustCompile(`^([0-9]+|0x[0-9a-f]*)$`)

type approvalLink struct {
	ServerIdentity string
	JobID          string
	Generation     uint64
}

func waitingJobApprovalLink(job JobRecord) (approvalLink, bool) {
	if job.Kind != jobKindUpdate || job.Status != jobStatusWaitingApproval {
		return approvalLink{}, false
	}
	var plan struct {
		ServerIdentity string `json:"server_identity"`
		Generation     uint64 `json:"approval_generation"`
	}
	if err := json.Unmarshal([]byte(job.MetaJSON), &plan); err != nil {
		return approvalLink{}, false
	}
	link := approvalLink{ServerIdentity: plan.ServerIdentity, JobID: job.ID, Generation: plan.Generation}
	return link, link.valid()
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
	if strings.ContainsRune(host, ':') {
		addr, err := netip.ParseAddr(host)
		return err == nil && addr.Is6() && addr.Zone() == ""
	}
	ascii, err := applicationOriginIDNA.ToASCII(host)
	name := strings.TrimSuffix(ascii, ".")
	if err != nil || !applicationOriginDNSName.MatchString(name) {
		return false
	}
	// Browsers interpret a hostname ending in a number as IPv4. Use canonical
	// dotted decimal so a shorthand or octal address can't change the destination.
	if browserIPv4Label.MatchString(name[strings.LastIndexByte(name, '.')+1:]) {
		addr, err := netip.ParseAddr(name)
		return err == nil && addr.Is4()
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

func statusReturnDestination(raw string) string {
	// Rebuild only the supported deep links. Never redirect to the raw input.
	target, err := url.ParseRequestURI(raw)
	if err != nil || target.IsAbs() || target.Host != "" || target.EscapedPath() != "/" {
		return "/"
	}
	query, err := url.ParseQuery(target.RawQuery)
	if err != nil {
		return "/"
	}
	if len(query) == 1 && len(query["maintenance_batch"]) == 1 && validBatchID(query.Get("maintenance_batch")) {
		return (&url.URL{Path: "/", RawQuery: query.Encode()}).String()
	}
	if link, valid := parseApprovalLink(query); valid {
		return link.url(url.URL{})
	}
	return "/"
}

func authPageDestination(path, returnTo string) string {
	if returnTo == "/" {
		return path
	}
	return path + "?" + url.Values{"return_to": {returnTo}}.Encode()
}

type statusAuthView struct {
	ReturnTo  string
	LoginURL  string
	SetupURL  string
	SubmitURL string
}

func newStatusAuthView(raw, submitPath string) statusAuthView {
	returnTo := statusReturnDestination(raw)
	return statusAuthView{
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
			return approvalLinkExpired
		case status.ApprovalGeneration != link.Generation:
			return approvalLinkReplaced
		default:
			return ""
		}
	}
	return approvalLinkMissingHost
}

func approvalLinkProblemAtOpen(deps AppDeps, link approvalLink) string {
	if problem := approvalLinkProblem(deps.ServerState, link); problem != "" {
		return problem
	}
	jm := deps.CurrentJobManager()
	if jm == nil {
		return approvalLinkExpired
	}
	job, err := jm.GetJob(link.JobID)
	if err != nil {
		return approvalLinkExpired
	}
	current, valid := waitingJobApprovalLink(job)
	if !valid || current.JobID != link.JobID {
		return approvalLinkExpired
	}
	if current.ServerIdentity != link.ServerIdentity {
		return approvalLinkMissingHost
	}
	if current.Generation != link.Generation {
		return approvalLinkReplaced
	}
	return ""
}
