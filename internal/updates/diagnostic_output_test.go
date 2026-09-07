package updates

import (
	"errors"
	"strings"
	"testing"

	"debian-updater/internal/jobs"
	"debian-updater/internal/servers"
)

func TestOutputBufferRetainsHeadAndTailAcrossChunkBoundaries(t *testing.T) {
	for _, chunk := range []int{1, 7, 64, 1000} {
		input := strings.Repeat("0123456789abcdef", 127)
		buffer := OutputBuffer{Limit: 64}
		for start := 0; start < len(input); start += chunk {
			part := input[start:min(start+chunk, len(input))]
			if n, err := buffer.WriteString(part); n != len(part) || err != nil {
				t.Fatalf("drain=%d/%v", n, err)
			}
		}
		want := input[:32] + OutputTruncationMarker + input[len(input)-32:]
		if !buffer.Truncated || buffer.String() != want {
			t.Fatalf("chunk=%d output=%q want=%q", chunk, buffer.String(), want)
		}
		if len(buffer.head)+len(buffer.tail) > 64 {
			t.Fatal("retained output exceeds limit")
		}
	}
}

func TestUnknownMutationOutcomeTakesPrecedenceOverSudoOutput(t *testing.T) {
	state, _ := testState()
	runner := &withActorRunner{service: NewService(ServiceDeps{ServerState: state, CurrentJobManager: func() *jobs.Manager { return nil }}), server: servers.Server{Name: "srv", User: "operator"}}
	runner.setCommandErrorLogs("Removing package...\nsudo: a password is required", NonRetryableTaggedError{Err: errors.New("connection reset"), ReconciliationRequired: true})
	if status := state.CurrentStatusSnapshot("srv").Status; status != "needs_reconciliation" || runner.lastErrClass != "reconciliation_required" {
		t.Fatalf("uncertain outcome masked by diagnostics: %s/%s", status, runner.lastErrClass)
	}
}

func TestOutputBufferPreservesCompleteSmallOutput(t *testing.T) {
	for _, size := range []int{0, 31, 32, 63, 64} {
		buffer := OutputBuffer{Limit: 64}
		input := strings.Repeat("x", size)
		_, _ = buffer.WriteString(input)
		if buffer.Truncated || buffer.String() != input {
			t.Fatalf("size=%d output=%q", size, buffer.String())
		}
	}
}

func TestLiveStatusLogsRemainBoundedDuringStreaming(t *testing.T) {
	state, _ := testState()
	server := servers.Server{Name: "srv"}
	service := NewService(ServiceDeps{ServerState: state, CurrentJobManager: func() *jobs.Manager { return nil }})
	runner := &withActorRunner{service: service, server: server}
	for range 128 {
		runner.appendLiveStatusLogs([]HostCommandOutput{{Stream: HostCommandStdout, Data: strings.Repeat("x", 4096)}})
	}
	if logs := state.CurrentStatusLogs(server.Name); len(logs) > LiveStatusLogLimit || !strings.Contains(logs, OutputTruncationMarker) {
		t.Fatalf("unbounded or unmarked logs: length=%d", len(logs))
	}
}
