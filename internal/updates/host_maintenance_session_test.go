package updates

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"debian-updater/internal/servers"

	"golang.org/x/crypto/ssh"
)

type maintenanceTestConnection struct {
	closed int
}

type inspectionExitError struct {
	code int
}

func (e inspectionExitError) Error() string   { return "exit status" }
func (e inspectionExitError) ExitStatus() int { return e.code }

type inspectionCommandResult struct {
	stdout string
	stderr string
	err    error
}

type inspectionTestConnection struct {
	results  map[string]inspectionCommandResult
	commands []string
}

func (*inspectionTestConnection) NewSession() (SSHSessionRunner, error) {
	return nil, errors.New("unused")
}

func (*inspectionTestConnection) Close() error { return nil }

func newInspectionSession(t *testing.T, conn *inspectionTestConnection) HostMaintenanceSession {
	t.Helper()
	factory := NewProductionHostMaintenanceSessionFactory(ProductionHostMaintenanceSessionDeps{
		BuildAuthMethods: func(servers.Server) ([]ssh.AuthMethod, error) {
			return []ssh.AuthMethod{ssh.Password("secret")}, nil
		},
		HostKeyCallback: func() (ssh.HostKeyCallback, error) {
			return ssh.InsecureIgnoreHostKey(), nil
		},
		DialSSH: func(servers.Server, *ssh.ClientConfig) (SSHConnection, error) {
			return conn, nil
		},
		RunCommand: func(_ context.Context, got SSHConnection, command string, _ io.Reader, _ time.Duration) (string, string, error) {
			if got != conn {
				t.Fatalf("command connection = %T, want inspection connection", got)
			}
			conn.commands = append(conn.commands, command)
			result, ok := conn.results[command]
			if !ok {
				return "", "", errors.New("unexpected command: " + command)
			}
			return result.stdout, result.stderr, result.err
		},
	})
	session, err := factory.Open(context.Background(), HostMaintenanceSessionRequest{
		Server:         servers.Server{Name: "srv", User: "root"},
		RetryPolicy:    RetryPolicy{MaxAttempts: 1},
		DialOperation:  "inspection.ssh_dial",
		CommandTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func TestProductionHostMaintenanceSessionOwnsPreUpdateInspection(t *testing.T) {
	commands := []string{
		"df -Pk /var / | awk 'NR>1 {print $2, $4}'",
		RootOrSudoCommand("/usr/bin/fuser /var/lib/dpkg/lock-frontend /var/lib/dpkg/lock /var/cache/apt/archives/lock"),
		RootOrSudoCommand("dpkg --audit"),
		RootOrSudoCommand("apt-get check"),
	}
	conn := &inspectionTestConnection{results: map[string]inspectionCommandResult{
		commands[0]: {stdout: "2097152 2048000\n3145728 2097152\n"},
		commands[1]: {err: inspectionExitError{code: 1}},
		commands[2]: {},
		commands[3]: {},
	}}

	summary := newInspectionSession(t, conn).RunUpdatePrechecks(context.Background())
	if !summary.AllPassed || summary.FailedCheck != "" || len(summary.Results) != 3 {
		t.Fatalf("RunUpdatePrechecks() = %+v, want three passing checks", summary)
	}
	if !reflect.DeepEqual(conn.commands, commands) {
		t.Fatalf("commands = %#v, want %#v", conn.commands, commands)
	}
}

func TestProductionHostMaintenanceSessionOwnsPostUpdateInspection(t *testing.T) {
	failedUnitsCommand := "systemctl --failed --no-legend --plain"
	conn := &inspectionTestConnection{results: map[string]inspectionCommandResult{
		RootOrSudoCommand("dpkg --audit"):  {},
		RootOrSudoCommand("apt-get check"): {},
		failedUnitsCommand:                 {},
		"sh -c \"if [ -f /var/run/reboot-required ]; then echo required; fi\"": {stdout: "required\n"},
	}}
	session := newInspectionSession(t, conn)

	baseline, _, err := session.ListFailedSystemdUnits(context.Background())
	if err != nil || len(baseline) != 0 {
		t.Fatalf("ListFailedSystemdUnits() = %#v, %v, want empty baseline", baseline, err)
	}
	conn.results[failedUnitsCommand] = inspectionCommandResult{stdout: "ssh.service loaded failed failed\n"}
	summary := session.RunPostUpdateHealthChecks(context.Background(), PostUpdateCheckConfig{
		Enabled:               true,
		BlockOnAptHealth:      true,
		BlockOnFailedUnits:    true,
		RebootRequiredWarning: true,
	}, map[string]struct{}{})
	if summary.AllPassed || summary.FailedCheck != PostcheckNameFailedUnits || summary.Warnings != 1 || len(summary.Results) != 3 {
		t.Fatalf("RunPostUpdateHealthChecks() = %+v, want blocking new unit plus reboot warning", summary)
	}
}

func TestProductionHostMaintenanceSessionOwnsPackageDiscoveryAndCVEQueries(t *testing.T) {
	cveCommand := BuildPackageCVEQueryCmd("openssl")
	conn := &inspectionTestConnection{results: map[string]inspectionCommandResult{
		AptListUpgradableCmd: {stdout: "Inst openssl [1.0] (1.1 Ubuntu:24.04-security [amd64])\n"},
		AptFullUpgradeSimCmd: {stdout: "0 upgraded, 0 newly installed, 0 to remove and 0 not upgraded.\n"},
		AptListMetadataCmd:   {stdout: "openssl/noble-security 1.1 amd64 [upgradable from: 1.0]\n"},
		cveCommand:           {stdout: "CVE-2026-4101\nCVE-2026-4101\nCVE-2026-4102\n"},
	}}
	session := newInspectionSession(t, conn)

	discovery, err := session.DiscoverPackages(context.Background(), HostOperationRequest{Operation: "test.discovery"})
	if err != nil {
		t.Fatalf("DiscoverPackages() error = %v", err)
	}
	if discovery.Attempts != 1 || discovery.Outcome.PendingPackageCount != 1 || discovery.Outcome.SecurityPackageCount != 1 {
		t.Fatalf("DiscoverPackages() = %+v, want one security update", discovery)
	}
	cves, err := session.QueryPackageCVEs(context.Background(), "openssl")
	if err != nil {
		t.Fatalf("QueryPackageCVEs() error = %v", err)
	}
	if !reflect.DeepEqual(cves, []string{"CVE-2026-4101", "CVE-2026-4102"}) {
		t.Fatalf("QueryPackageCVEs() = %#v", cves)
	}
}

func TestProductionHostMaintenanceSessionCVEQueriesPreserveLimitAndFailure(t *testing.T) {
	t.Run("limits unique CVEs", func(t *testing.T) {
		lines := make([]string, 0, CVELookupMaxPerPackage+2)
		for index := 0; index < CVELookupMaxPerPackage+2; index++ {
			lines = append(lines, fmt.Sprintf("CVE-2026-%04d", index+1))
		}
		command := BuildPackageCVEQueryCmd("openssl")
		conn := &inspectionTestConnection{results: map[string]inspectionCommandResult{command: {stdout: strings.Join(lines, "\n")}}}
		cves, err := newInspectionSession(t, conn).QueryPackageCVEs(context.Background(), "openssl")
		if err != nil || len(cves) != CVELookupMaxPerPackage {
			t.Fatalf("QueryPackageCVEs() = %#v, %v", cves, err)
		}
	})

	t.Run("returns command failure", func(t *testing.T) {
		command := BuildPackageCVEQueryCmd("openssl")
		conn := &inspectionTestConnection{results: map[string]inspectionCommandResult{command: {err: errors.New("query failed")}}}
		if _, err := newInspectionSession(t, conn).QueryPackageCVEs(context.Background(), "openssl"); err == nil {
			t.Fatal("QueryPackageCVEs() error = nil, want command failure")
		}
	})
}

func TestProductionHostMaintenanceSessionOwnsHostFactProbing(t *testing.T) {
	conn := &inspectionTestConnection{results: map[string]inspectionCommandResult{
		"sh -c '. /etc/os-release 2>/dev/null; printf \"%s\\n\" \"${PRETTY_NAME:-unknown}\"'": {stdout: "Ubuntu 24.04 LTS\n"},
		hostFactsRunningKernelCmd:         {stdout: "6.8.0-60-generic\n"},
		hostFactsLatestInstalledKernelCmd: {stdout: "6.8.0-62-generic\n"},
		"cat /proc/uptime":                {stdout: "3600.50 100.00\n"},
		precheckDiskSpaceCmd:              {stdout: "41943040 2097152\n10485760 3145728\n"},
		precheckDpkgAuditCmd:              {},
		precheckAptCheckCmd:               {},
		postcheckRebootCmd:                {stdout: "required\n"},
	}}
	session := newInspectionSession(t, conn)

	facts := session.CollectServerFacts(context.Background())
	if facts.ServerName != "srv" || facts.OSPrettyName != "Ubuntu 24.04 LTS" || facts.UptimeSeconds != 3600 {
		t.Fatalf("CollectServerFacts() identity = %+v", facts)
	}
	if facts.RunningKernelVersion != "6.8.0-60-generic" || facts.LatestInstalledKernelVersion != "6.8.0-62-generic" {
		t.Fatalf("CollectServerFacts() kernel versions = %+v", facts)
	}
	if facts.DiskStatus != "ok" || facts.DiskFreeKB != 2097152 || facts.DiskTotalKB != 41943040 {
		t.Fatalf("CollectServerFacts() disk = %+v", facts)
	}
	if facts.AptStatus != "ok" || facts.RebootRequired == nil || !*facts.RebootRequired {
		t.Fatalf("CollectServerFacts() health = %+v", facts)
	}
	diskProbeCount := 0
	for _, command := range conn.commands {
		if command == precheckDiskSpaceCmd {
			diskProbeCount++
		}
	}
	if diskProbeCount != 1 {
		t.Fatalf("disk probe executions = %d, want 1", diskProbeCount)
	}
}

func TestProductionHostMaintenanceSessionPreUpdateFailuresAreSafeAndOrdered(t *testing.T) {
	tests := []struct {
		name        string
		results     map[string]inspectionCommandResult
		failedCheck string
		detail      string
		commands    int
	}{
		{
			name: "malformed disk output",
			results: map[string]inspectionCommandResult{
				precheckDiskSpaceCmd: {stdout: "not-a-number\n"},
			},
			failedCheck: "disk_space",
			detail:      "Invalid free space value",
			commands:    1,
		},
		{
			name: "low disk space",
			results: map[string]inspectionCommandResult{
				precheckDiskSpaceCmd: {stdout: "1048575\n"},
			},
			failedCheck: "disk_space",
			detail:      "minimum 1.00 GiB",
			commands:    1,
		},
		{
			name: "lock check requires passwordless sudo",
			results: map[string]inspectionCommandResult{
				precheckDiskSpaceCmd: {stdout: "2097152\n"},
				precheckLocksCmd:     {stderr: "sudo: a password is required", err: inspectionExitError{code: 1}},
			},
			failedCheck: "apt_locks",
			detail:      "passwordless sudo",
			commands:    2,
		},
		{
			name: "active package-manager lock",
			results: map[string]inspectionCommandResult{
				precheckDiskSpaceCmd: {stdout: "2097152\n"},
				precheckLocksCmd:     {stdout: "4312\n"},
			},
			failedCheck: "apt_locks",
			detail:      "currently in use",
			commands:    2,
		},
		{
			name: "missing fuser command",
			results: map[string]inspectionCommandResult{
				precheckDiskSpaceCmd: {stdout: "2097152\n"},
				precheckLocksCmd:     {stderr: "/usr/bin/fuser: command not found", err: inspectionExitError{code: 127}},
			},
			failedCheck: "apt_locks",
			detail:      "Install package `psmisc`",
			commands:    2,
		},
		{
			name: "dpkg audit reports unhealthy state",
			results: map[string]inspectionCommandResult{
				precheckDiskSpaceCmd: {stdout: "2097152\n"},
				precheckLocksCmd:     {err: inspectionExitError{code: 1}},
				precheckDpkgAuditCmd: {stdout: "package is only half configured\n"},
			},
			failedCheck: "apt_health",
			detail:      "package state issues",
			commands:    3,
		},
		{
			name: "apt health command fails",
			results: map[string]inspectionCommandResult{
				precheckDiskSpaceCmd: {stdout: "2097152\n"},
				precheckLocksCmd:     {err: inspectionExitError{code: 1}},
				precheckDpkgAuditCmd: {},
				precheckAptCheckCmd:  {stderr: "unmet dependencies", err: inspectionExitError{code: 100}},
			},
			failedCheck: "apt_health",
			detail:      "apt-get check failed",
			commands:    4,
		},
		{
			name: "inspection timeout does not replay",
			results: map[string]inspectionCommandResult{
				precheckDiskSpaceCmd: {err: errors.New("command timed out after 1s")},
			},
			failedCheck: "disk_space",
			detail:      "timed out",
			commands:    1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn := &inspectionTestConnection{results: tt.results}
			summary := newInspectionSession(t, conn).RunUpdatePrechecks(context.Background())
			if summary.AllPassed || summary.FailedCheck != tt.failedCheck {
				t.Fatalf("RunUpdatePrechecks() = %+v", summary)
			}
			if got := summary.Results[len(summary.Results)-1].Details; !strings.Contains(got, tt.detail) {
				t.Fatalf("failure detail = %q, want %q", got, tt.detail)
			}
			if len(conn.commands) != tt.commands {
				t.Fatalf("commands = %#v, want %d commands", conn.commands, tt.commands)
			}
		})
	}
}

func TestProductionHostMaintenanceSessionHonorsCancellationAfterInspectionStarts(t *testing.T) {
	started := make(chan struct{})
	factory := NewProductionHostMaintenanceSessionFactory(ProductionHostMaintenanceSessionDeps{
		BuildAuthMethods: func(servers.Server) ([]ssh.AuthMethod, error) { return nil, nil },
		HostKeyCallback:  func() (ssh.HostKeyCallback, error) { return ssh.InsecureIgnoreHostKey(), nil },
		DialSSH: func(servers.Server, *ssh.ClientConfig) (SSHConnection, error) {
			return &inspectionTestConnection{}, nil
		},
		RunCommand: func(ctx context.Context, _ SSHConnection, _ string, _ io.Reader, _ time.Duration) (string, string, error) {
			close(started)
			<-ctx.Done()
			return "", "", ctx.Err()
		},
	})
	session, err := factory.Open(context.Background(), HostMaintenanceSessionRequest{Server: servers.Server{User: "root"}, RetryPolicy: RetryPolicy{MaxAttempts: 1}, CommandTimeout: time.Minute})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	resultCh := make(chan PrecheckSummary, 1)
	go func() { resultCh <- session.RunUpdatePrechecks(ctx) }()
	<-started
	cancel()
	select {
	case summary := <-resultCh:
		if summary.AllPassed || !strings.Contains(summary.Results[0].Details, context.Canceled.Error()) {
			t.Fatalf("RunUpdatePrechecks(cancelled in flight) = %+v", summary)
		}
	case <-time.After(time.Second):
		t.Fatal("in-flight inspection did not honor context cancellation")
	}
}

func TestProductionHostMaintenanceSessionDeadlineInterruptsRetryBackoff(t *testing.T) {
	conn := &maintenanceTestConnection{}
	sleepStarted := make(chan struct{})
	releaseSleep := make(chan struct{})
	commands := 0
	factory := NewProductionHostMaintenanceSessionFactory(ProductionHostMaintenanceSessionDeps{
		BuildAuthMethods: func(servers.Server) ([]ssh.AuthMethod, error) { return nil, nil },
		HostKeyCallback:  func() (ssh.HostKeyCallback, error) { return ssh.InsecureIgnoreHostKey(), nil },
		DialSSH:          func(servers.Server, *ssh.ClientConfig) (SSHConnection, error) { return conn, nil },
		RunCommand: func(context.Context, SSHConnection, string, io.Reader, time.Duration) (string, string, error) {
			commands++
			return "", "mirror temporarily unavailable", errors.New("exit status 100")
		},
		Sleep: func(time.Duration) {
			close(sleepStarted)
			<-releaseSleep
		},
	})
	session, err := factory.Open(context.Background(), HostMaintenanceSessionRequest{
		Server:         servers.Server{User: "root"},
		RetryPolicy:    RetryPolicy{MaxAttempts: 3, BaseDelay: time.Minute, MaxDelay: time.Minute},
		CommandTimeout: time.Minute,
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	errCh := make(chan error, 1)
	go func() {
		_, runErr := session.RunCommand(ctx, HostCommandRequest{Operation: "test.retry", Command: "apt-get update", ReplayPolicy: ReplayRetryableOutputErrors})
		errCh <- runErr
	}()
	select {
	case <-sleepStarted:
	case <-time.After(time.Second):
		t.Fatal("retry backoff did not start")
	}
	err = <-errCh
	close(releaseSleep)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RunCommand() error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("deadline cancellation took %s, want under one second", elapsed)
	}
	if commands != 1 || session.Stats().OperationAttempts["test.retry"] != 1 {
		t.Fatalf("commands=%d stats=%+v, want one non-replayed attempt", commands, session.Stats())
	}
}

func TestProductionHostMaintenanceSessionStreamsRequestedCommandOutput(t *testing.T) {
	conn := &maintenanceTestConnection{}
	fallbackCalls := 0
	streamingCalls := 0
	factory := NewProductionHostMaintenanceSessionFactory(ProductionHostMaintenanceSessionDeps{
		BuildAuthMethods: func(servers.Server) ([]ssh.AuthMethod, error) { return nil, nil },
		HostKeyCallback:  func() (ssh.HostKeyCallback, error) { return ssh.InsecureIgnoreHostKey(), nil },
		DialSSH:          func(servers.Server, *ssh.ClientConfig) (SSHConnection, error) { return conn, nil },
		RunCommand: func(context.Context, SSHConnection, string, io.Reader, time.Duration) (string, string, error) {
			fallbackCalls++
			return "fallback", "", nil
		},
		RunStreamingCommand: func(_ context.Context, _ SSHConnection, _ string, _ io.Reader, _ time.Duration, onOutput HostCommandOutputHandler) (string, string, error) {
			streamingCalls++
			onOutput(HostCommandOutput{Stream: HostCommandStdout, Data: "unpacking\n"})
			onOutput(HostCommandOutput{Stream: HostCommandStderr, Data: "configuration warning\n"})
			return "unpacking\n", "configuration warning\n", nil
		},
	})
	session, err := factory.Open(context.Background(), HostMaintenanceSessionRequest{
		Server:         servers.Server{User: "root"},
		RetryPolicy:    RetryPolicy{MaxAttempts: 1},
		CommandTimeout: time.Minute,
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	var outputs []HostCommandOutput
	attemptCompletions := 0
	result, err := session.RunCommand(context.Background(), HostCommandRequest{
		Operation: "test.stream",
		Command:   "apt-get -y upgrade",
		OnOutput: func(output HostCommandOutput) {
			outputs = append(outputs, output)
		},
		OnAttemptComplete: func() {
			attemptCompletions++
		},
	})
	if err != nil {
		t.Fatalf("RunCommand() error = %v", err)
	}
	if fallbackCalls != 0 || streamingCalls != 1 {
		t.Fatalf("fallback calls=%d streaming calls=%d, want 0/1", fallbackCalls, streamingCalls)
	}
	if result.Stdout != "unpacking\n" || result.Stderr != "configuration warning\n" {
		t.Fatalf("RunCommand() result = %+v", result)
	}
	want := []HostCommandOutput{
		{Stream: HostCommandStdout, Data: "unpacking\n"},
		{Stream: HostCommandStderr, Data: "configuration warning\n"},
	}
	if !reflect.DeepEqual(outputs, want) {
		t.Fatalf("streamed outputs = %#v, want %#v", outputs, want)
	}
	if attemptCompletions != 1 {
		t.Fatalf("attempt completions = %d, want 1", attemptCompletions)
	}
}

func TestProductionHostMaintenanceSessionCompletesStreamAttemptBeforeRetryNotification(t *testing.T) {
	first := &maintenanceTestConnection{}
	second := &maintenanceTestConnection{}
	connections := []SSHConnection{first, second}
	dials := 0
	attempts := 0
	var events []string
	factory := NewProductionHostMaintenanceSessionFactory(ProductionHostMaintenanceSessionDeps{
		BuildAuthMethods: func(servers.Server) ([]ssh.AuthMethod, error) { return nil, nil },
		HostKeyCallback:  func() (ssh.HostKeyCallback, error) { return ssh.InsecureIgnoreHostKey(), nil },
		DialSSH: func(servers.Server, *ssh.ClientConfig) (SSHConnection, error) {
			conn := connections[dials]
			dials++
			return conn, nil
		},
		RunCommand: func(context.Context, SSHConnection, string, io.Reader, time.Duration) (string, string, error) {
			return "", "", errors.New("unexpected fallback command")
		},
		RunStreamingCommand: func(_ context.Context, _ SSHConnection, _ string, _ io.Reader, _ time.Duration, onOutput HostCommandOutputHandler) (string, string, error) {
			attempts++
			output := fmt.Sprintf("attempt-%d\n", attempts)
			onOutput(HostCommandOutput{Stream: HostCommandStdout, Data: output})
			events = append(events, "output:"+strings.TrimSpace(output))
			if attempts == 1 {
				return output, "", RetryableTaggedError{Err: errors.New("temporary transport failure")}
			}
			return output, "", nil
		},
		Sleep: func(time.Duration) {},
	})
	session, err := factory.Open(context.Background(), HostMaintenanceSessionRequest{
		Server:         servers.Server{User: "root"},
		RetryPolicy:    RetryPolicy{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
		CommandTimeout: time.Minute,
		OnRetry: func(HostRetryEvent) {
			events = append(events, "retry")
		},
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	_, err = session.RunCommand(context.Background(), HostCommandRequest{
		Operation: "test.stream-retry",
		Command:   "apt-get -y upgrade",
		OnOutput:  func(HostCommandOutput) {},
		OnAttemptComplete: func() {
			events = append(events, "flush")
		},
	})
	if err != nil {
		t.Fatalf("RunCommand() error = %v", err)
	}
	want := []string{"output:attempt-1", "flush", "retry", "output:attempt-2", "flush"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("attempt event order = %#v, want %#v", events, want)
	}
}

func TestProductionHostMaintenanceSessionInspectionHonorsCancelledContext(t *testing.T) {
	conn := &inspectionTestConnection{results: map[string]inspectionCommandResult{}}
	session := newInspectionSession(t, conn)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	summary := session.RunUpdatePrechecks(ctx)
	if summary.AllPassed || summary.FailedCheck != "disk_space" || !strings.Contains(summary.Results[0].Details, context.Canceled.Error()) {
		t.Fatalf("RunUpdatePrechecks(cancelled) = %+v", summary)
	}
	if len(conn.commands) != 0 {
		t.Fatalf("cancelled inspection ran commands: %#v", conn.commands)
	}
}

func TestProductionHostMaintenanceSessionBoundsInspectionOutputByRune(t *testing.T) {
	conn := &inspectionTestConnection{results: map[string]inspectionCommandResult{
		precheckDiskSpaceCmd: {stdout: strings.Repeat("é", inspectionOutputMaxLen+20)},
	}}
	summary := newInspectionSession(t, conn).RunUpdatePrechecks(context.Background())
	if summary.AllPassed || len(summary.Results) != 1 {
		t.Fatalf("RunUpdatePrechecks() = %+v", summary)
	}
	output := summary.Results[0].Output
	if !utf8.ValidString(output) || len([]rune(output)) != inspectionOutputMaxLen {
		t.Fatalf("bounded output has %d runes and valid UTF-8=%t", len([]rune(output)), utf8.ValidString(output))
	}
	details := summary.Results[0].Details
	if !utf8.ValidString(details) || len([]rune(details)) > inspectionOutputMaxLen {
		t.Fatalf("bounded details have %d runes and valid UTF-8=%t", len([]rune(details)), utf8.ValidString(details))
	}
}

func TestProductionHostMaintenanceSessionPostUpdatePreservesBaselineAndCustomFailure(t *testing.T) {
	customCommand := "sudo /usr/local/bin/health-check"
	conn := &inspectionTestConnection{results: map[string]inspectionCommandResult{
		precheckDpkgAuditCmd:    {},
		precheckAptCheckCmd:     {},
		postcheckFailedUnitsCmd: {stdout: "postfix.service loaded failed failed\n"},
		customCommand:           {stderr: "health endpoint failed", err: inspectionExitError{code: 1}},
	}}
	summary := newInspectionSession(t, conn).RunPostUpdateHealthChecks(context.Background(), PostUpdateCheckConfig{
		Enabled:            true,
		BlockOnAptHealth:   true,
		BlockOnFailedUnits: true,
		CustomCommand:      customCommand,
	}, map[string]struct{}{"postfix.service": {}})
	if summary.AllPassed || summary.FailedCheck != PostcheckNameCustomCmd || summary.Warnings != 0 || len(summary.Results) != 3 {
		t.Fatalf("RunPostUpdateHealthChecks() = %+v", summary)
	}
	if !summary.Results[1].Passed || !strings.Contains(summary.Results[1].Details, "pre-existing") {
		t.Fatalf("failed-unit baseline result = %+v", summary.Results[1])
	}
}

func TestProductionHostMaintenanceSessionPostUpdateDisabledAndBlockingAPT(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		conn := &inspectionTestConnection{results: map[string]inspectionCommandResult{}}
		summary := newInspectionSession(t, conn).RunPostUpdateHealthChecks(context.Background(), PostUpdateCheckConfig{}, nil)
		if !summary.AllPassed || len(summary.Results) != 0 || len(conn.commands) != 0 {
			t.Fatalf("disabled RunPostUpdateHealthChecks() = %+v, commands=%#v", summary, conn.commands)
		}
	})

	t.Run("apt health blocks", func(t *testing.T) {
		conn := &inspectionTestConnection{results: map[string]inspectionCommandResult{
			precheckDpkgAuditCmd: {stderr: "database damaged", err: inspectionExitError{code: 1}},
		}}
		summary := newInspectionSession(t, conn).RunPostUpdateHealthChecks(context.Background(), PostUpdateCheckConfig{Enabled: true, BlockOnAptHealth: true}, nil)
		if summary.AllPassed || summary.FailedCheck != PostcheckNameAptHealth || len(summary.Results) != 2 {
			t.Fatalf("blocking APT RunPostUpdateHealthChecks() = %+v", summary)
		}
	})
}

func TestProductionHostMaintenanceSessionHostFactsDegradeIndependently(t *testing.T) {
	conn := &inspectionTestConnection{results: map[string]inspectionCommandResult{
		hostFactsOSCmd:       {stdout: "Debian GNU/Linux 13\n"},
		hostFactsUptimeCmd:   {stdout: "not-a-number\n"},
		precheckDiskSpaceCmd: {err: errors.New("disk probe failed")},
		precheckDpkgAuditCmd: {err: errors.New("apt probe failed")},
		postcheckRebootCmd:   {err: errors.New("reboot probe failed")},
	}}
	facts := newInspectionSession(t, conn).CollectServerFacts(context.Background())
	if facts.OSPrettyName != "Debian GNU/Linux 13" || facts.UptimeSeconds != 0 {
		t.Fatalf("identity facts = %+v", facts)
	}
	if facts.DiskStatus != "unknown" || facts.AptStatus != "unknown" || facts.RebootRequired != nil {
		t.Fatalf("partial health facts = %+v", facts)
	}
	if !strings.Contains(facts.RawJSON, "disk probe failed") || !strings.Contains(facts.RawJSON, "reboot probe failed") {
		t.Fatalf("RawJSON = %s", facts.RawJSON)
	}
}

func (*maintenanceTestConnection) NewSession() (SSHSessionRunner, error) {
	return nil, errors.New("unused")
}

func (c *maintenanceTestConnection) Close() error {
	c.closed++
	return nil
}

func TestProductionHostMaintenanceSessionOwnsReconnectRetryAndClose(t *testing.T) {
	server := servers.Server{Name: "srv", User: "root"}
	first := &maintenanceTestConnection{}
	second := &maintenanceTestConnection{}
	connections := []SSHConnection{first, second}
	dials := 0
	commands := 0
	stdinBuilds := 0
	var events []HostRetryEvent

	factory := NewProductionHostMaintenanceSessionFactory(ProductionHostMaintenanceSessionDeps{
		BuildAuthMethods: func(got servers.Server) ([]ssh.AuthMethod, error) {
			if got.Name != server.Name {
				t.Fatalf("server = %q, want %q", got.Name, server.Name)
			}
			return []ssh.AuthMethod{ssh.Password("secret")}, nil
		},
		HostKeyCallback: func() (ssh.HostKeyCallback, error) {
			return ssh.InsecureIgnoreHostKey(), nil
		},
		DialSSH: func(servers.Server, *ssh.ClientConfig) (SSHConnection, error) {
			conn := connections[dials]
			dials++
			return conn, nil
		},
		RunCommand: func(_ context.Context, conn SSHConnection, cmd string, stdin io.Reader, _ time.Duration) (string, string, error) {
			commands++
			if _, err := io.ReadAll(stdin); err != nil {
				t.Fatalf("read command stdin: %v", err)
			}
			if commands == 1 {
				if conn != first {
					t.Fatalf("first command connection = %T, want first", conn)
				}
				return "", "mirror temporarily unavailable", errors.New("exit status 100")
			}
			if conn != second {
				t.Fatalf("second command connection = %T, want replacement", conn)
			}
			return "updated", "", nil
		},
		Sleep: func(time.Duration) {},
	})

	session, err := factory.Open(context.Background(), HostMaintenanceSessionRequest{
		Server:         server,
		RetryPolicy:    RetryPolicy{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
		DialOperation:  "update.ssh_dial",
		CommandTimeout: time.Second,
		OnRetry: func(event HostRetryEvent) {
			events = append(events, event)
		},
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	result, err := session.RunCommand(context.Background(), HostCommandRequest{
		Operation: "update.apt_update",
		Command:   AptUpdateCmd,
		Stdin: func() io.Reader {
			stdinBuilds++
			return strings.NewReader("password")
		},
		ReplayPolicy: ReplayRetryableOutputErrors,
	})
	if err != nil {
		t.Fatalf("RunCommand() error = %v", err)
	}
	if result.Stdout != "updated" || result.Attempts != 2 {
		t.Fatalf("RunCommand() = %+v, want updated after two attempts", result)
	}
	if stdinBuilds != 2 {
		t.Fatalf("stdin builds = %d, want one fresh reader per attempt", stdinBuilds)
	}
	if len(events) != 1 || events[0].Operation != "update.apt_update" || events[0].Attempt != 1 {
		t.Fatalf("retry events = %+v, want one apt-update retry", events)
	}
	stats := session.Stats()
	if stats.DialAttempts != 1 || stats.OperationAttempts["update.apt_update"] != 2 || stats.Reconnects != 1 {
		t.Fatalf("Stats() = %+v", stats)
	}
	if first.closed != 1 {
		t.Fatalf("first connection close count = %d, want 1 during reconnect", first.closed)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	if second.closed != 1 {
		t.Fatalf("replacement close count = %d, want idempotent close", second.closed)
	}
}
